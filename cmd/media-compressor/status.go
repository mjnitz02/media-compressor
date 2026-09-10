package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// runStatus reports what the database knows, without touching a single file.
//
// This is the audit surface until the web UI arrives, and it is written for
// somebody who last looked six months ago: what is outstanding, what was
// declined and why, what went wrong and is being retried, and what the whole
// thing has actually saved.
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	limit := fs.Int("limit", 15, "how many recent jobs and blocked files to list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.Paths.Database); err != nil {
		fmt.Printf("no database at %s yet: nothing has been scanned.\n", cfg.Paths.Database)
		return nil
	}

	db, err := store.Open(cfg.Paths.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	return printStatus(context.Background(), os.Stdout, db, cfg.Paths.Database, *limit, time.Now())
}

func printStatus(ctx context.Context, w io.Writer, db *store.Store, dbPath string, limit int, now time.Time) error {
	counts, err := db.Counts(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "%s\n\n", dbPath)
	fmt.Fprintf(w, "library\n")
	fmt.Fprintf(w, "  %6d files seen\n", counts.Files)
	fmt.Fprintf(w, "  %6d decided\n", counts.Decided)
	fmt.Fprintf(w, "  %6d leave alone -- already as their profile wants them\n", counts.ByAction[decide.ActionNone])
	fmt.Fprintf(w, "  %6d remux outstanding\n", counts.ByAction[decide.ActionRemux])
	fmt.Fprintf(w, "  %6d encode outstanding\n", counts.ByAction[decide.ActionEncode])
	// The number worth watching. The stack this replaces declined 7,880 files
	// on the bitrate floor, and that is the main reason its output always
	// looked good -- it refused the hard jobs. If this number moves a long way
	// after a config change, that change was a quality change.
	fmt.Fprintf(w, "  %6d encode declined on the bitrate floor\n", counts.SkippedFloor)

	fmt.Fprintf(w, "\nwork done\n")
	fmt.Fprintf(w, "  %6d finished\n", counts.JobsDone)
	fmt.Fprintf(w, "  %6d failed\n", counts.JobsFailed)
	fmt.Fprintf(w, "  %6s reclaimed\n", humanBytes(counts.BytesSaved))

	blocked, err := db.BlockedFiles(ctx, limit)
	if err != nil {
		return err
	}
	if len(blocked) > 0 {
		fmt.Fprintf(w, "\nheld back after a failure (%d) -- these are the ones worth reading\n", counts.Blocked)
		for _, f := range blocked {
			_, why := f.Blocked(now)
			if why == "" {
				why = f.LastError
			}
			fmt.Fprintf(w, "  %s\n%s\n", f.Path, indent(why))
		}
		if counts.Blocked > len(blocked) {
			fmt.Fprintf(w, "  ... and %d more\n", counts.Blocked-len(blocked))
		}
	}

	jobs, err := db.RecentJobs(ctx, limit)
	if err != nil {
		return err
	}
	if len(jobs) > 0 {
		fmt.Fprintf(w, "\nrecent work\n")
		for _, j := range jobs {
			fmt.Fprintf(w, "  %s  %-6s %-6s %s\n",
				j.StartedAt.Format("2006-01-02 15:04"), j.Action, j.Status, filepath.Base(j.Path))
			switch {
			case j.Status == store.StatusDone && j.SizeAfter > 0:
				fmt.Fprintf(w, "      %s -> %s in %s\n",
					humanBytes(j.SizeBefore), humanBytes(j.SizeAfter), j.Elapsed.Round(time.Second))
			case j.Error != "":
				fmt.Fprintf(w, "%s\n", indent(j.Error))
			}
		}
	}

	if counts.Files == 0 {
		fmt.Fprintf(w, "\nnothing has been scanned yet. Run `media-compressor scan -library NAME`.\n")
	}
	return nil
}
