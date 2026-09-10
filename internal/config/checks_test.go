package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The remaining validation branches, one line each. They exist to catch
// fat-finger edits to config.yaml, and every one of them would otherwise be
// discovered from the tool's behaviour rather than from its startup.

func TestProfileFieldsAreRequired(t *testing.T) {
	cases := []struct{ name, remove, want string }{
		{"target codec", "      target_codec: hevc\n", "video.target_codec"},
		{"encoder", "      encoder: libx265\n", "video.encoder"},
		{"multichannel target", "      multichannel_to: ac3\n", "audio.multichannel_to"},
		{"tiers", "        tiers:\n          - { above: 3000, divisor: 1.5 }\n          - { above: 0, divisor: 1.0 }\n", "tiers"},
		{"min multiplier", "        min_multiplier: 0.7\n", "min_multiplier"},
		{"max multiplier", "        max_multiplier: 1.3\n", "max_multiplier"},
		{"listen address", "  listen: \":8080\"\n", ""}, // has a built-in default
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := replace(t, c.remove, "")
			if c.want == "" {
				mustParse(t, src)
				return
			}
			wantProblem(t, src, c.want)
		})
	}
}

func TestMultichannelThresholdMustBeSensible(t *testing.T) {
	wantProblem(t, replace(t, "multichannel_threshold: 6", "multichannel_threshold: 1"), "at least 2")
}

func TestMinMultiplierAboveMaxIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "        min_multiplier: 0.7", "        min_multiplier: 1.9"), "above max_multiplier")
}

func TestNegativeTierThresholdIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "{ above: 3000, divisor: 1.5 }", "{ above: -1, divisor: 1.5 }"), "negative")
}

func TestZeroDivisorIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "{ above: 3000, divisor: 1.5 }", "{ above: 3000, divisor: 0 }"), "greater than 0")
}

func TestNegativeFloorIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "floor_kbps: 3000", "floor_kbps: -1"), "negative")
}

func TestUnknownBitrateBasisIsAnError(t *testing.T) {
	src := replace(t, "      target_codec: hevc", "      target_codec: hevc\n      source_bitrate_basis: filesize")
	wantProblem(t, src, "source_bitrate_basis")
}

func TestProfileLevelContainerIsChecked(t *testing.T) {
	src := replace(t, "  standard:\n", "  standard:\n    container: matroska\n")
	wantProblem(t, src, "profiles.standard.container")
}

func TestScannerTimingIsChecked(t *testing.T) {
	wantProblem(t, replace(t, "min_age_seconds: 3600", "min_age_seconds: -1"), "negative")
	wantProblem(t, replace(t, "interval_minutes: 180", "interval_minutes: -5"), "interval_minutes")
}

func TestBadIgnoreGlobIsAnError(t *testing.T) {
	src := replace(t, "  min_age_seconds: 3600", "  ignore_globs: [\"[\"]\n  min_age_seconds: 3600")
	wantProblem(t, src, "valid pattern")
}

func TestNoProfilesIsAnError(t *testing.T) {
	_, err := Parse([]byte("libraries:\n  - name: a\n    paths: [/mnt/a]\n"))
	if err == nil {
		t.Fatal("a config with no profiles should not parse")
	}
}

func TestCheckPathsRejectsAFileWhereADirectoryIsExpected(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "movies")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := mustParse(t, replace(t, "[/mnt/movies]", "["+notADir+"]"))
	if err := c.CheckPaths(); err == nil {
		t.Fatal("a regular file passed as a library path")
	}
}

func TestCheckPathsChecksWorkDirAndDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	src := replace(t, "[/mnt/movies]", "["+dir+"]")
	c := mustParse(t, src)

	// Both /temp and /config are missing on a development machine, which is
	// exactly the situation -check-paths=false exists for.
	if err := c.CheckPaths(); err == nil {
		t.Fatal("a missing work_dir and database directory should be reported")
	}

	ok := mustParse(t, replaceAll(t, src,
		"work_dir: /temp", "work_dir: "+dir,
		"database: /config/db.sqlite", "database: "+filepath.Join(dir, "state.db")))
	if err := ok.CheckPaths(); err != nil {
		t.Fatalf("existing directories were reported as missing: %v", err)
	}
}
