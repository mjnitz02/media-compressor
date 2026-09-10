// Command media-compressor keeps a media library encoded as HEVC with
// unwanted tracks removed.
//
// `validate`, `plan` and `run` exist. The background scanner and the web UI
// are later phases; see docs/plan.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/decide"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `media-compressor - keep a media library as HEVC, without the junk tracks

usage: media-compressor <command> [flags]

commands:
  validate    read the config, resolve every profile, and report problems
  plan        print exactly what would happen to a library, and touch nothing
  run         do it
  version     print the version

plan and run take either -library NAME from the config, or one or more paths:

  media-compressor plan -library movies
  media-compressor plan /mnt/media_video/movies/Some.Film.2019.mkv
  media-compressor run -library movies -limit 5

"plan" is "run -dry-run": the same code path stopped one step short of
writing anything, so what it prints is the work itself rather than a
description of it. Start there.

run "media-compressor <command> -h" for that command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "validate":
		err = runValidate(os.Args[2:])
	case "plan":
		err = runRun(os.Args[2:], true)
	case "run":
		err = runRun(os.Args[2:], false)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	path := fs.String("config", defaultConfigPath(), "path to config.yaml")
	checkPaths := fs.Bool("check-paths", true, "also check that every library path exists (use -check-paths=false when validating a server's config from elsewhere)")
	quiet := fs.Bool("quiet", false, "print nothing on success")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	// Path checking is separate from parsing so that a config for a server
	// can be checked from a laptop where none of the mounts exist.
	var pathErr error
	if *checkPaths {
		pathErr = cfg.CheckPaths()
	}

	if !*quiet {
		printSummary(os.Stdout, *path, cfg)
	}
	if pathErr != nil {
		return pathErr
	}
	return nil
}

// defaultConfigPath prefers a config.yaml next to the binary's working
// directory, falling back to the container layout.
func defaultConfigPath() string {
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	return filepath.Join("/config", "config.yaml")
}

// printSummary is the whole point of the command: what will actually happen to
// each library, stated plainly enough to check by eye.
func printSummary(w *os.File, path string, cfg *config.Config) {
	fmt.Fprintf(w, "%s parsed cleanly.\n", path)

	fmt.Fprintf(w, "\nlibraries\n")
	for _, lib := range cfg.Libraries {
		plex := "notifies plex"
		if !lib.NotifyPlex {
			plex = "no plex notification"
		}
		fmt.Fprintf(w, "  %-10s profile %-10s container %-7s %s\n",
			lib.Name, lib.ProfileName, lib.Profile.Container, plex)
		for _, p := range lib.Paths {
			fmt.Fprintf(w, "  %-10s   %s\n", "", p)
		}
	}

	fmt.Fprintf(w, "\nprofiles\n")
	for _, name := range sortedProfileNames(cfg) {
		describeProfile(w, cfg.Profiles[name])
	}

	fmt.Fprintf(w, "\nruntime\n")
	fmt.Fprintf(w, "  workers      %d remux, %d encode\n", cfg.Defaults.Workers.Remux, cfg.Defaults.Workers.Encode)
	fmt.Fprintf(w, "  scan         every %d min, files must be %d s old and size-stable\n",
		cfg.Scanner.IntervalMinutes, cfg.Scanner.MinAgeSeconds)
	fmt.Fprintf(w, "  extensions   %s\n", strings.Join(cfg.Scanner.Extensions, " "))
	fmt.Fprintf(w, "  work dir     %s\n", cfg.Paths.WorkDir)
	fmt.Fprintf(w, "  database     %s\n", cfg.Paths.Database)
	fmt.Fprintf(w, "  listen       %s\n", cfg.Server.Listen)

	if len(cfg.Warnings) > 0 {
		fmt.Fprintf(w, "\nworth knowing\n")
		for _, warn := range cfg.Warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}
}

func describeProfile(w *os.File, p decide.Profile) {
	fmt.Fprintf(w, "  %s\n", p.Name)
	fmt.Fprintf(w, "    video      %s via %s, leaving %s alone\n",
		p.Video.TargetCodec, p.Video.Encoder, strings.Join(p.Video.LeaveAlone, "/"))
	fmt.Fprintf(w, "    bitrate    %s basis; %s; %.2gx-%.2gx; skip below %d kbps\n",
		p.Video.Bitrate.Basis, describeTiers(p.Video.Bitrate.Tiers),
		p.Video.Bitrate.MinMultiplier, p.Video.Bitrate.MaxMultiplier, p.Video.Bitrate.FloorKbps)
	fmt.Fprintf(w, "    audio      keep %s%s; %d+ channels to %s%s\n",
		strings.Join(p.Audio.KeepLanguages, ","), untaggedNote(p.Audio.KeepUntagged),
		p.Audio.MultichannelThreshold, p.Audio.MultichannelTo, tagNote(p.Audio.TagUntaggedAs))
	fmt.Fprintf(w, "               %s; drop titles matching %s\n",
		describeTagTrust(p.Audio.PrimaryLanguages), strings.Join(p.Audio.DropTitlesMatching, ","))
	fmt.Fprintf(w, "    subtitles  keep %s%s%s%s\n",
		strings.Join(p.Subtitles.KeepLanguages, ","), untaggedNote(p.Subtitles.KeepUntagged),
		forcedNote(p.Subtitles.KeepForced), tagNote(p.Subtitles.TagUntaggedAs))
	fmt.Fprintf(w, "    strip      %s\n", describeStrip(p.Strip))
}

func describeTiers(tiers []decide.Tier) string {
	parts := make([]string, 0, len(tiers))
	for _, t := range tiers {
		parts = append(parts, strconv.Itoa(t.AboveKbps)+"+ /"+trimFloat(t.Divisor))
	}
	return strings.Join(parts, ", ")
}

// describeTagTrust states the gate on language-based dropping: with no
// primary languages named, the tags are always believed.
func describeTagTrust(primary []string) string {
	if len(primary) == 0 {
		return "language tags always believed"
	}
	return "trust tags only when " + strings.Join(primary, "/") + " present"
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func untaggedNote(keep bool) string {
	if keep {
		return " + untagged"
	}
	return ""
}

func forcedNote(keep bool) string {
	if keep {
		return "; forced always kept"
	}
	return ""
}

func tagNote(lang string) string {
	if lang == "" {
		return ""
	}
	return "; tag untagged as " + lang
}

func describeStrip(s decide.StripRules) string {
	var on []string
	if s.DataStreams {
		on = append(on, "data streams")
	}
	if s.CoverArt {
		on = append(on, "cover art")
	}
	if s.Chapters {
		on = append(on, "chapters")
	}
	if len(on) == 0 {
		return "nothing"
	}
	return strings.Join(on, ", ")
}

func sortedProfileNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	// sort.Strings without importing sort twice; the list is tiny.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}
