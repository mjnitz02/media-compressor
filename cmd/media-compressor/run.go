package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/probe"
	"github.com/mjnitz02/media-compressor/internal/queue"
	"github.com/mjnitz02/media-compressor/internal/runner"
	"github.com/mjnitz02/media-compressor/internal/scan"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// runPass implements plan, scan and run. They are one code path with the mode
// as a parameter, which is the point: --dry-run is not a simulation of the
// real thing, it is the real thing stopped one step short. What plan prints is
// the Job the encoder would have been handed.
func runPass(args []string, mode runner.Mode) error {
	fs := flag.NewFlagSet(string(mode), flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	library := fs.String("library", "", "work over every path in this library from the config")
	profileName := fs.String("profile", "", "force this profile instead of the one the library implies")
	limit := fs.Int("limit", 0, "stop after this many files that need work (0 = no limit)")
	verbose := fs.Bool("v", false, "list every declined and waiting file, and print the ffmpeg command for each job")
	ffmpegBin := fs.String("ffmpeg", encode.DefaultBinary, "ffmpeg binary")
	ffprobeBin := fs.String("ffprobe", probe.DefaultBinary, "ffprobe binary")
	unsettled := fs.Bool("unsettled", false,
		"act on files that have not yet been seen unchanged by a second scan")

	var retryFailed *bool
	if mode != runner.ModePlan {
		retryFailed = fs.Bool("retry-failed", false,
			"try files again that a previous run gave up on")
	}
	dryRun := mode == runner.ModePlan
	if mode == runner.ModeRun {
		fs.BoolVar(&dryRun, "dry-run", false, "print the full plan and touch nothing")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if dryRun {
		mode = runner.ModePlan
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	targets, err := resolveTargets(cfg, *library, *profileName, fs.Args())
	if err != nil {
		return err
	}

	db, err := openStore(cfg, mode)
	if err != nil {
		return err
	}
	if db != nil {
		defer db.Close()
	}

	// Ctrl-C stops after the files in flight rather than mid-encode, and the
	// encoder deletes its temp file on cancellation. A second one is the
	// process default and kills it outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if mode == runner.ModeRun {
		checkWorkDir(cfg, targets, os.Stderr)
	}

	opts := runner.Options{
		Mode:             mode,
		Limit:            *limit,
		Workers:          queue.Workers{Remux: cfg.Defaults.Workers.Remux, Encode: cfg.Defaults.Workers.Encode},
		ProbeWorkers:     cfg.Defaults.Workers.Remux,
		IncludeUnsettled: *unsettled,
		RetryFailed:      retryFailed != nil && *retryFailed,
	}

	failed := 0
	for _, t := range targets {
		n, err := runOneTarget(ctx, cfg, db, t, opts, *ffmpegBin, *ffprobeBin, *verbose)
		if err != nil {
			return err
		}
		failed += n
	}
	if failed > 0 {
		return fmt.Errorf("%d %s could not be processed; the originals are untouched",
			failed, plural(failed, "file", "files"))
	}
	return nil
}

// openStore opens the state database.
//
// A dry run opens it read-only in spirit -- it reads sightings and cached
// decisions but writes nothing -- and will not create the file if it does not
// exist yet. Printing a plan should not leave anything behind.
func openStore(cfg *config.Config, mode runner.Mode) (*store.Store, error) {
	path := cfg.Paths.Database
	if mode == runner.ModePlan {
		if _, err := os.Stat(path); err != nil {
			return nil, nil
		}
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("database directory: %w", err)
		}
	}
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if mode == runner.ModeRun {
		// A job still marked running belongs to a process that died. Nothing
		// was replaced -- that only happens after verification -- so the row
		// is merely untidy, and leaving it would make the history lie.
		if n, err := db.AbandonRunningJobs(context.Background(), time.Now()); err != nil {
			db.Close()
			return nil, err
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "note: %d %s left running by a previous process, closed off\n",
				n, plural(n, "job was", "jobs were"))
		}
	}
	return db, nil
}

// checkWorkDir settles once, at startup, whether the configured work dir can
// actually be used, and drops it from the config for this process when it
// cannot.
//
// It is checked here rather than discovered per file because the failure it
// prevents happens at the very last step of an encode -- the rename that
// replaces the original -- and therefore costs the entire encode, every time,
// for every file. Dropping the work dir puts the temp file beside the source
// instead, which is what an unconfigured work dir does and is always safe.
//
// Only "run" and "daemon" ask: scanning writes no media, and a dry run may
// not write at all.
func checkWorkDir(cfg *config.Config, targets []runner.Target, w io.Writer) {
	if cfg.Paths.WorkDir == "" {
		return
	}
	for _, t := range targets {
		for _, root := range t.Roots {
			// A target can name a file rather than a directory, because
			// somebody can point this at a single file.
			dir := root
			if info, err := os.Stat(dir); err == nil && !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			err := encode.CanRenameInto(cfg.Paths.WorkDir, dir)
			if err == nil {
				continue
			}
			fmt.Fprintf(w, workDirUnusable, cfg.Paths.WorkDir, dir, err)
			// This process's answer to "where does the temp file go", not a
			// change to anybody's configuration file.
			cfg.Paths.WorkDir = ""
			return
		}
	}
}

const workDirUnusable = `note: not using the work dir %s: a file in it cannot be renamed into %s
  (%v)
  In-progress encodes will go beside the file being replaced instead, which is
  always safe and is exactly what happens with no work dir configured at all.
  A work dir only helps when it is on the same *mount* as the media -- and in a
  container, a /temp mounted separately from the media never is.
`

// newRunner assembles the collaborators for one config.
func newRunner(cfg *config.Config, db *store.Store, ffmpegBin, ffprobeBin string) *runner.Runner {
	prober := probe.Prober{Binary: ffprobeBin}
	return &runner.Runner{
		Store:  db,
		Prober: prober,
		Encoder: encode.Encoder{
			Binary:  ffmpegBin,
			Prober:  prober,
			WorkDir: cfg.Paths.WorkDir,
		},
		Scan: scan.Options{
			Extensions:  cfg.Scanner.Extensions,
			IgnoreGlobs: cfg.Scanner.IgnoreGlobs,
			MinAge:      time.Duration(cfg.Scanner.MinAgeSeconds) * time.Second,
		},
	}
}

// runOneTarget runs one pass and prints it, returning the number of files that
// failed outright.
//
// "Failed" is narrower than "was not processed". A file this tool declines --
// a symlink it will not replace in place, a plan decide could not make -- is
// reported and moves on, because declining is a valid result and a library
// with one odd file in it should not make every run exit non-zero. Only a
// file that could not be read, or work that went wrong, counts.
func runOneTarget(ctx context.Context, cfg *config.Config, db *store.Store, t runner.Target,
	opts runner.Options, ffmpegBin, ffprobeBin string, verbose bool) (int, error) {

	// Sweeping forgets files that are no longer on disk, which is only a
	// correct conclusion when whole libraries were walked. A pass over paths
	// somebody named would otherwise decide the rest of the library had
	// vanished.
	opts.Sweep = !t.Named

	r := newRunner(cfg, db, ffmpegBin, ffprobeBin)
	live := newLiveOutput(os.Stdout, verbose)
	r.OnEvent = live.handle

	header := t.Label
	if opts.Mode == runner.ModePlan {
		header = "dry run: " + t.Label
	}

	sum, err := r.Pass(ctx, t, opts)
	live.finish()
	if err != nil {
		return 0, err
	}

	printPass(os.Stdout, header, t, sum, opts, verbose, ffmpegBin)
	return sum.Failures(), nil
}

// printPass writes what one pass did. It is separate from running the pass
// because the daemon runs passes from its own loop and prints them the same
// way -- a run in the terminal and a run three months into a daemon's life
// should read identically.
func printPass(w io.Writer, header string, t runner.Target, sum *runner.Summary,
	opts runner.Options, verbose bool, ffmpegBin string) {

	for _, werr := range sum.WalkErrors {
		fmt.Fprintf(os.Stderr, "warning: %v\n", werr)
	}

	rep := &report{title: header, verbose: verbose, binary: ffmpegBin}
	if len(t.Roots) == 1 {
		rep.root = t.Roots[0]
	}
	rep.addSummary(sum)
	rep.print(w, opts.Mode == runner.ModePlan)

	if sum.StoppedAtLimit {
		fmt.Fprintf(w, "\nstopped at -limit %d; the rest of the library was left for the next run.\n", opts.Limit)
	}
	if sum.Cancelled {
		fmt.Fprintf(w, "\nstopped early: the run was interrupted, so the rest of the library was left alone.\n")
	}
	switch opts.Mode {
	case runner.ModePlan:
		fmt.Fprintf(w, "\nnothing was written. Re-run the same command as `run` to carry this out.\n")
	case runner.ModeScan:
		fmt.Fprintf(w, "\nno media was touched; what was found has been recorded. Run `run` to carry it out.\n")
	}
}

// resolveTargets works out what to operate on and, crucially, under which
// profile. A path that belongs to no library and has no -profile is an error
// rather than a guess: silently encoding a folder with the wrong quality
// target is the failure this whole config package exists to prevent.
func resolveTargets(cfg *config.Config, library, profileName string, paths []string) ([]runner.Target, error) {
	if library != "" && len(paths) > 0 {
		return nil, errors.New("give either -library or some paths, not both")
	}

	if library != "" {
		for i := range cfg.Libraries {
			lib := &cfg.Libraries[i]
			if lib.Name != library {
				continue
			}
			prof := lib.Profile
			if profileName != "" {
				p, ok := cfg.Profiles[profileName]
				if !ok {
					return nil, fmt.Errorf("profile %q is not defined", profileName)
				}
				prof = p
			}
			return []runner.Target{{
				Label:   fmt.Sprintf("library %s (profile %s, container %s)", lib.Name, prof.Name, prof.Container),
				Library: lib.Name,
				Roots:   lib.Paths,
				Profile: prof,
			}}, nil
		}
		return nil, fmt.Errorf("library %q is not defined in the config", library)
	}

	if len(paths) == 0 {
		return nil, errors.New("nothing to do: pass -library NAME, or one or more paths")
	}

	var targets []runner.Target
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, err
		}

		var prof decide.Profile
		var libName string
		switch {
		case profileName != "":
			p, ok := cfg.Profiles[profileName]
			if !ok {
				return nil, fmt.Errorf("profile %q is not defined", profileName)
			}
			prof = p
		default:
			l, ok := cfg.LibraryFor(abs)
			if !ok {
				return nil, fmt.Errorf("%s is not inside any configured library, so there is no profile for it; "+
					"pass -profile NAME to say which rules to apply", abs)
			}
			libName, prof = l.Name, l.Profile
		}

		label := fmt.Sprintf("%s (profile %s, container %s)", abs, prof.Name, prof.Container)
		if libName != "" {
			label = fmt.Sprintf("%s (library %s, profile %s, container %s)", abs, libName, prof.Name, prof.Container)
		}
		// Named: somebody typed this path, so they are not waiting for the
		// file to finish arriving.
		targets = append(targets, runner.Target{
			Label: label, Library: libName, Roots: []string{abs}, Profile: prof, Named: true,
		})
	}
	return targets, nil
}

// liveOutput turns runner events into something worth watching.
//
// Two shapes, because there are two audiences. A single job in a terminal gets
// one line rewritten in place, which is what you want when you are sitting
// there. Anything else -- several workers, or output going to a log the daemon
// will be read from months later -- gets plain lines with timestamps' worth of
// context in them, because a carriage return in a log file is noise.
type liveOutput struct {
	mu      sync.Mutex
	w       io.Writer
	verbose bool

	inline bool
	active bool
	last   map[int]time.Time
}

// progressEvery is how often a job reports itself in line mode. Often enough
// to show a four-hour encode is alive, rare enough not to fill a log.
const progressEvery = 30 * time.Second

func newLiveOutput(w io.Writer, verbose bool) *liveOutput {
	return &liveOutput{w: w, verbose: verbose, last: map[int]time.Time{}}
}

func (l *liveOutput) handle(e runner.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch e.Phase {
	case runner.PhaseQueued:
		// One job in a terminal is the case worth animating.
		l.inline = e.Count == 1 && isTerminal(os.Stdout)

	case runner.PhaseStart:
		if !l.inline {
			fmt.Fprintf(l.w, "  %s %s\n", verbFor(e.Item), filepath.Base(e.Item.Path))
		}

	case runner.PhaseProgress:
		now := time.Now()
		if l.inline {
			if now.Sub(l.last[e.Item.Index]) < time.Second && !e.Progress.Done {
				return
			}
			l.last[e.Item.Index] = now
			l.active = true
			fmt.Fprintf(l.w, "\r  %s %s ... %3.0f%% (%.1fx)\033[K",
				verbFor(e.Item), truncateLabel(filepath.Base(e.Item.Path), 50),
				e.Progress.Fraction*100, e.Progress.Speed)
			return
		}
		if now.Sub(l.last[e.Item.Index]) < progressEvery {
			return
		}
		l.last[e.Item.Index] = now
		fmt.Fprintf(l.w, "  %s %s ... %3.0f%% (%.1fx)\n",
			verbFor(e.Item), filepath.Base(e.Item.Path), e.Progress.Fraction*100, e.Progress.Speed)

	case runner.PhaseDone:
		l.clear()
		delete(l.last, e.Item.Index)
		if l.inline {
			return
		}
		if e.Err != nil {
			fmt.Fprintf(l.w, "  %s failed -- the original is untouched\n", filepath.Base(e.Item.Path))
			return
		}
		if e.Result != nil {
			fmt.Fprintf(l.w, "  %s %s\n", filepath.Base(e.Item.Path), describeResult(e.Result))
		}

	case runner.PhaseWarning:
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", e.Path, e.Err)
	}
}

// finish clears any half-written progress line.
func (l *liveOutput) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
}

func (l *liveOutput) clear() {
	if l.active {
		fmt.Fprint(l.w, "\r\033[K")
		l.active = false
	}
}

func verbFor(it *queue.Item) string {
	if it != nil && it.Kind == queue.KindEncode {
		return "encoding"
	}
	return "remuxing"
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func truncateLabel(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
