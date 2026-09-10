package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/queue"
	"github.com/mjnitz02/media-compressor/internal/runner"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// runDaemon is how this is meant to be left: a loop that scans every library
// on the configured interval and carries out whatever it finds.
//
// Two properties matter more than anything clever it could do instead.
//
// It must never wedge. Every failure here is reported and stepped over -- a
// library that cannot be read, a file ffprobe chokes on, an encode that goes
// wrong -- because the alternative is a service that stopped making progress
// in March and nobody noticed until September.
//
// And it must be interruptible. SIGTERM finishes the files in flight and stops
// starting new ones, which matters because Unraid stops containers with one.
func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	once := fs.Bool("once", false, "make a single pass over every library and exit")
	interval := fs.Duration("interval", 0, "override scanner.interval_minutes")
	verbose := fs.Bool("v", false, "list every declined and waiting file")
	ffmpegBin := fs.String("ffmpeg", "", "ffmpeg binary")
	ffprobeBin := fs.String("ffprobe", "", "ffprobe binary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.CheckPaths(); err != nil {
		// Starting a long-running service pointed at mounts that are not
		// there would mean sweeping every file out of the database as
		// "deleted" -- so this one is worth refusing to start over.
		return err
	}
	if len(cfg.Libraries) == 0 {
		return errors.New("no libraries are configured, so there is nothing to watch")
	}

	every := time.Duration(cfg.Scanner.IntervalMinutes) * time.Minute
	if *interval > 0 {
		every = *interval
	}

	db, err := openStore(cfg, runner.ModeRun)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := runner.Options{
		Mode:         runner.ModeRun,
		Workers:      queue.Workers{Remux: cfg.Defaults.Workers.Remux, Encode: cfg.Defaults.Workers.Encode},
		ProbeWorkers: cfg.Defaults.Workers.Remux,
		Sweep:        true,
	}

	for {
		started := time.Now()
		daemonPass(ctx, cfg, db, opts, *ffmpegBin, *ffprobeBin, *verbose)
		if *once || ctx.Err() != nil {
			return nil
		}

		// Measured from the end of the pass, not the start: an encode that
		// took six hours should not be followed immediately by another scan.
		wait := every - time.Since(started)
		if wait < time.Minute {
			wait = time.Minute
		}
		fmt.Printf("\nnext scan in %s\n", wait.Round(time.Minute))

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			fmt.Println("stopping")
			return nil
		}
	}
}

// daemonPass runs every library once. A library that fails is reported and the
// rest still run.
func daemonPass(ctx context.Context, cfg *config.Config, db *store.Store, opts runner.Options,
	ffmpegBin, ffprobeBin string, verbose bool) {

	for i := range cfg.Libraries {
		if ctx.Err() != nil {
			return
		}
		lib := &cfg.Libraries[i]
		t := runner.Target{
			Label: fmt.Sprintf("library %s (profile %s, container %s)",
				lib.Name, lib.Profile.Name, lib.Profile.Container),
			Library: lib.Name,
			Roots:   lib.Paths,
			Profile: lib.Profile,
		}
		if _, err := runOneTarget(ctx, cfg, db, t, opts, ffmpegBin, ffprobeBin, verbose); err != nil {
			fmt.Fprintf(os.Stderr, "library %s: %v\n", lib.Name, err)
		}
	}
}
