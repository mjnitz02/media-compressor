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
	"github.com/mjnitz02/media-compressor/internal/daemon"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/probe"
	"github.com/mjnitz02/media-compressor/internal/queue"
	"github.com/mjnitz02/media-compressor/internal/runner"
)

// runDaemon is how this is meant to be left: a loop that scans every library
// on the configured interval, carries out whatever it finds, and serves a page
// showing what it is doing.
//
// The loop itself lives in internal/daemon, because the web UI has to be able
// to watch it and to ask it for a pass, and neither is possible against a
// local variable in here.
func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	once := fs.Bool("once", false, "make a single pass over every library and exit")
	interval := fs.Duration("interval", 0, "override scanner.interval_minutes")
	verbose := fs.Bool("v", false, "list every declined and waiting file")
	ffmpegBin := fs.String("ffmpeg", encode.DefaultBinary, "ffmpeg binary")
	ffprobeBin := fs.String("ffprobe", probe.DefaultBinary, "ffprobe binary")
	listen := fs.String("listen", "", "override server.listen for the web UI")
	noWeb := fs.Bool("no-web", false, "do not serve the web UI")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The address the holding page uses if there turns out to be no config to
	// read one from. The -listen flag wins over both.
	waitAddr := config.DefaultListen
	if *listen != "" {
		waitAddr = *listen
	}
	if *noWeb {
		waitAddr = ""
	}

	var cfg *config.Config
	if *once {
		// A single pass is a one-shot: it is run by hand or by a timer, and
		// something is reading its exit status. Waiting would hang it.
		var err error
		if cfg, err = loadUsableConfig(*cfgPath); err != nil {
			return err
		}
	} else {
		var err error
		if cfg, err = waitForConfig(ctx, *cfgPath, waitAddr, os.Stdout); err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Println("stopping")
				return nil
			}
			return err
		}
	}

	every := time.Duration(cfg.Scanner.IntervalMinutes) * time.Minute
	if *interval > 0 {
		every = *interval
	}

	targets := libraryTargets(cfg)
	checkWorkDir(cfg, targets, os.Stderr)

	db, err := openStore(cfg, runner.ModeRun)
	if err != nil {
		return err
	}
	defer db.Close()

	opts := runner.Options{
		Mode:         runner.ModeRun,
		Workers:      queue.Workers{Remux: cfg.Defaults.Workers.Remux, Encode: cfg.Defaults.Workers.Encode},
		ProbeWorkers: cfg.Defaults.Workers.Remux,
		Sweep:        true,
	}

	live := newLiveOutput(os.Stdout, *verbose)
	d := &daemon.Daemon{
		Runner:   newRunner(cfg, db, *ffmpegBin, *ffprobeBin),
		Targets:  targets,
		Options:  opts,
		Interval: every,
		OnEvent:  live.handle,
		OnPass: func(t runner.Target, sum *runner.Summary, err error) {
			live.finish()
			if err != nil {
				// Reported and stepped over. One library that cannot be read
				// must not stop the others, or the next pass.
				fmt.Fprintf(os.Stderr, "library %s: %v\n", t.Library, err)
				return
			}
			printPass(os.Stdout, t.Label, t, sum, opts, *verbose, *ffmpegBin)
		},
	}

	addr := cfg.Server.Listen
	if *listen != "" {
		addr = *listen
	}
	if !*noWeb && addr != "" {
		// A UI that cannot bind is worth complaining about loudly, but it is
		// not worth refusing to encode over: this process's job is the media,
		// and the page is how you watch it.
		srv, err := startWeb(cfg, db, d, addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: the web UI could not start: %v\n"+
				"the daemon is running without it\n", err)
		} else {
			fmt.Printf("web UI on http://%s\n", addr)
			defer shutdownWeb(srv)
		}
	}

	if *once {
		d.RunOnce(ctx)
		return nil
	}
	d.Run(ctx)
	fmt.Println("stopping")
	return nil
}

// libraryTargets is every configured library as something the runner can be
// pointed at.
func libraryTargets(cfg *config.Config) []runner.Target {
	targets := make([]runner.Target, 0, len(cfg.Libraries))
	for i := range cfg.Libraries {
		lib := &cfg.Libraries[i]
		targets = append(targets, runner.Target{
			Label: fmt.Sprintf("library %s (profile %s, container %s)",
				lib.Name, lib.Profile.Name, lib.Profile.Container),
			Library: lib.Name,
			Roots:   lib.Paths,
			Profile: lib.Profile,
		})
	}
	return targets
}
