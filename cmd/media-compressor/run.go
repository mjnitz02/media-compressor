package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/probe"
	"github.com/mjnitz02/media-compressor/internal/scan"
)

// runRun implements both `run` and `plan`. They are the same code path with
// dryRun defaulting differently, which is the point: --dry-run is not a
// simulation of the real thing, it is the real thing stopped one step short.
// What it prints is the Job the encoder would have been handed.
func runRun(args []string, dryRun bool) error {
	name := "run"
	if dryRun {
		name = "plan"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	library := fs.String("library", "", "run over every path in this library from the config")
	profileName := fs.String("profile", "", "force this profile instead of the one the library implies")
	limit := fs.Int("limit", 0, "stop after this many files that need work (0 = no limit)")
	verbose := fs.Bool("v", false, "list every declined file, and print the ffmpeg command for each job")
	ffmpegBin := fs.String("ffmpeg", encode.DefaultBinary, "ffmpeg binary")
	ffprobeBin := fs.String("ffprobe", probe.DefaultBinary, "ffprobe binary")
	if !dryRun {
		fs.BoolVar(&dryRun, "dry-run", false, "print the full plan and touch nothing")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	targets, err := resolveTargets(cfg, *library, *profileName, fs.Args())
	if err != nil {
		return err
	}

	// Ctrl-C stops after the file in flight rather than mid-encode, and the
	// encoder deletes its temp file on cancellation. A second one is the
	// process default and kills it outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prober := probe.Prober{Binary: *ffprobeBin}
	encoder := encode.Encoder{
		Binary:  *ffmpegBin,
		Prober:  prober,
		WorkDir: cfg.Paths.WorkDir,
	}

	failed := 0
	for _, t := range targets {
		n, err := runTarget(ctx, cfg, t, encoder, prober, options{
			dryRun:  dryRun,
			verbose: *verbose,
			limit:   *limit,
			ffmpeg:  *ffmpegBin,
		})
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

type options struct {
	dryRun  bool
	verbose bool
	limit   int
	ffmpeg  string
}

// target is one set of paths and the profile that governs them.
type target struct {
	label   string
	roots   []string
	profile decide.Profile
	lib     *config.Library
}

// resolveTargets works out what to operate on and, crucially, under which
// profile. A path that belongs to no library and has no -profile is an error
// rather than a guess: silently encoding a folder with the wrong quality
// target is the failure this whole config package exists to prevent.
func resolveTargets(cfg *config.Config, library, profileName string, paths []string) ([]target, error) {
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
			return []target{{
				label:   fmt.Sprintf("library %s (profile %s, container %s)", lib.Name, prof.Name, prof.Container),
				roots:   lib.Paths,
				profile: prof,
				lib:     lib,
			}}, nil
		}
		return nil, fmt.Errorf("library %q is not defined in the config", library)
	}

	if len(paths) == 0 {
		return nil, errors.New("nothing to do: pass -library NAME, or one or more paths")
	}

	var targets []target
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, err
		}

		var prof decide.Profile
		var lib *config.Library
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
			lib, prof = l, l.Profile
		}

		label := fmt.Sprintf("%s (profile %s, container %s)", abs, prof.Name, prof.Container)
		if lib != nil {
			label = fmt.Sprintf("%s (library %s, profile %s, container %s)", abs, lib.Name, prof.Name, prof.Container)
		}
		targets = append(targets, target{label: label, roots: []string{abs}, profile: prof, lib: lib})
	}
	return targets, nil
}

// runTarget plans, and optionally carries out, the work for one target. It
// returns the number of files that failed outright.
//
// "Failed" is narrower than "was not processed". A file this tool declines --
// a symlink it will not replace in place, a plan decide could not make -- is
// reported and moves on, because declining is a valid result and a library
// with one odd file in it should not make every run exit non-zero. Only a
// file that could not be read, or an encode that went wrong, counts.
func runTarget(ctx context.Context, cfg *config.Config, t target, encoder encode.Encoder, prober probe.Prober, o options) (int, error) {
	header := "dry run: " + t.label
	if !o.dryRun {
		header = t.label
	}

	found := scan.Walk(t.roots, scan.Options{
		Extensions:  cfg.Scanner.Extensions,
		IgnoreGlobs: cfg.Scanner.IgnoreGlobs,
		MinAge:      time.Duration(cfg.Scanner.MinAgeSeconds) * time.Second,
	})

	rep := &report{title: header, verbose: o.verbose, binary: o.ffmpeg}
	if len(t.roots) == 1 {
		rep.root = t.roots[0]
	}
	for _, s := range found.Skipped {
		rep.unscanned = append(rep.unscanned, fmt.Sprintf("%s -- %s", s.Path, s.Reason))
	}
	for _, err := range found.Errors {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	worked, failures := 0, 0
	stoppedAtLimit := false

	for _, c := range found.Candidates {
		if ctx.Err() != nil {
			break
		}

		out := outcome{path: c.Path}
		r, err := prober.Run(ctx, c.Path)
		if err != nil {
			out.err = err
			failures++
			rep.add(out)
			continue
		}

		out.plan = decide.Decide(r, t.profile)
		switch out.plan.Action {
		case decide.ActionNone:
			// The most common outcome, and nothing to prepare.
		case decide.ActionError:
			out.declined = out.plan.Reason
		default:
			job, err := encoder.Prepare(r, out.plan)
			if err != nil {
				// Prepare only refuses for reasons that are about this file
				// being unsafe to replace, never about the encode itself.
				out.declined = err.Error()
			} else {
				out.job = job
			}
		}

		if out.job != nil && !o.dryRun {
			if o.limit > 0 && worked >= o.limit {
				stoppedAtLimit = true
				break
			}
			res, err := runOne(ctx, encoder, out.job)
			out.result = res
			if err != nil {
				out.err = err
				failures++
			}
			worked++
		}
		rep.add(out)
	}

	rep.print(os.Stdout, o.dryRun)
	if stoppedAtLimit {
		fmt.Fprintf(os.Stdout, "\nstopped at -limit %d; the rest of the library was not looked at.\n", o.limit)
	}
	if ctx.Err() != nil {
		fmt.Fprintf(os.Stdout, "\nstopped early: %v\n", ctx.Err())
	}
	if o.dryRun {
		fmt.Fprintf(os.Stdout, "\nnothing was written. Re-run the same command as `run` to carry this out.\n")
	}
	return failures, nil
}

// runOne encodes a single file, printing progress when stdout is a terminal.
func runOne(ctx context.Context, encoder encode.Encoder, job *encode.Job) (*encode.Result, error) {
	label := filepath.Base(job.SourcePath)
	verb := "remuxing"
	if job.Plan.Action == decide.ActionEncode {
		verb = "encoding"
	}

	if isTerminal(os.Stdout) {
		last := time.Time{}
		encoder.OnProgress = func(p encode.Progress) {
			// Once a second is enough; ffmpeg reports several times that
			// often and a busy terminal is its own kind of unreadable.
			if time.Since(last) < time.Second && !p.Done {
				return
			}
			last = time.Now()
			fmt.Fprintf(os.Stdout, "\r  %s %s ... %3.0f%% (%.1fx)\033[K", verb, truncateLabel(label, 50), p.Fraction*100, p.Speed)
		}
	}

	res, err := encoder.Run(ctx, job)
	if isTerminal(os.Stdout) {
		fmt.Fprint(os.Stdout, "\r\033[K")
	}
	return res, err
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
