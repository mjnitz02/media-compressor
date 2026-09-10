package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// minimal is the smallest configuration that passes validation. Tests build
// on it by string substitution, which keeps each test showing only the line it
// is actually about.
const minimal = `
defaults:
  profile: standard
  container: mkv
profiles:
  standard:
    video:
      target_codec: hevc
      encoder: libx265
      leave_alone: [hevc, av1]
      bitrate:
        tiers:
          - { above: 3000, divisor: 1.5 }
          - { above: 0, divisor: 1.0 }
        min_multiplier: 0.7
        max_multiplier: 1.3
        floor_kbps: 3000
    audio:
      multichannel_to: ac3
      multichannel_threshold: 6
      keep_languages: [eng]
      primary_languages: [eng]
    subtitles:
      keep_languages: [eng]
libraries:
  - name: movies
    paths: [/mnt/movies]
scanner:
  extensions: [mkv]
  min_age_seconds: 3600
  interval_minutes: 180
paths:
  work_dir: /temp
  database: /config/db.sqlite
server:
  listen: ":8080"
`

// mustParse fails the test if the config does not parse.
func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("expected a valid config, got:\n%v", err)
	}
	return c
}

// wantProblem asserts that parsing fails and that one of the reported
// problems mentions the given text. Matching on a substring keeps the tests
// about the situation rather than the exact wording.
func wantProblem(t *testing.T, src, substr string) {
	t.Helper()
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected a problem mentioning %q, but the config parsed cleanly", substr)
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected a *config.Error, got %T: %v", err, err)
	}
	for _, p := range ce.Problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("expected a problem mentioning %q, got:\n%v", substr, err)
}

// replace swaps the first occurrence of old in the minimal config, failing
// loudly if the anchor has drifted.
func replace(t *testing.T, old, new string) string {
	t.Helper()
	if !strings.Contains(minimal, old) {
		t.Fatalf("test anchor %q is no longer in the minimal config", old)
	}
	return strings.Replace(minimal, old, new, 1)
}

func TestMinimalConfigParses(t *testing.T) {
	c := mustParse(t, minimal)

	if len(c.Libraries) != 1 {
		t.Fatalf("got %d libraries, want 1", len(c.Libraries))
	}
	lib := c.Libraries[0]
	if lib.Name != "movies" || lib.ProfileName != "standard" {
		t.Errorf("library resolved to %+v", lib)
	}
	if lib.Profile.Container != decide.ContainerMKV {
		t.Errorf("container = %q, want mkv", lib.Profile.Container)
	}
	if !lib.NotifyPlex {
		t.Error("notify_plex should default to true")
	}
}

// The example config is the file an operator starts from, and the built-in
// profiles are the ones the golden corpus proves reproduce the old stack's
// output. If the two ever disagree, the shipped example silently encodes
// differently from the thing all 1,881 fixtures were checked against.
func TestExampleConfigReproducesTheBuiltinProfiles(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("config.example.yaml does not parse: %v", err)
	}

	for _, tc := range []struct {
		name string
		want decide.Profile
	}{
		{"standard", decide.Standard()},
		{"anime", decide.Anime()},
	} {
		got, ok := c.Profiles[tc.name]
		if !ok {
			t.Fatalf("config.example.yaml has no %q profile", tc.name)
		}
		if diff := profileDiff(got, tc.want); diff != "" {
			t.Errorf("profile %s differs from decide.%s():\n%s", tc.name,
				strings.ToUpper(tc.name[:1])+tc.name[1:], diff)
		}
	}
}

func TestLibraryProfileTypoIsAnError(t *testing.T) {
	src := replace(t, "  - name: movies", "  - name: movies\n    profile: standrad")
	// The whole point: a typo must never fall back to some other profile's
	// encode settings.
	wantProblem(t, src, `profile "standrad" is not defined`)
}

func TestDefaultProfileTypoIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "  profile: standard", "  profile: nope"), "defaults.profile")
}

func TestUnknownTopLevelFieldIsAnError(t *testing.T) {
	wantProblem(t, minimal+"\nnonsense: 1\n", "nonsense")
}

func TestUnknownFieldInsideAProfileIsAnError(t *testing.T) {
	src := replace(t, "    audio:\n      multichannel_to: ac3", "    audio:\n      multichanel_to: ac3")
	wantProblem(t, src, "multichanel_to")
}

// Quirks are bug-for-bug switches for the stack being replaced. Being able to
// turn one on in production would mean asking for a known bug.
func TestQuirksAreNotConfigurable(t *testing.T) {
	src := replace(t, "  standard:\n", "  standard:\n    quirks:\n      minute_factor: 0.0166667\n")
	wantProblem(t, src, "quirks")

	c := mustParse(t, minimal)
	if got := c.Profiles["standard"].Quirks.MinuteFactor; got != 1.0/60.0 {
		t.Errorf("minute factor = %v, want the corrected 1/60", got)
	}
	if c.Profiles["standard"].Audio.KeepAllLanguages {
		t.Error("keep_all_languages is a compatibility shim and must stay off")
	}
}

func TestNotifyPlexCanBeTurnedOff(t *testing.T) {
	src := replace(t, "  - name: movies", "  - name: movies\n    notify_plex: false")
	c := mustParse(t, src)
	if c.Libraries[0].NotifyPlex {
		t.Error("notify_plex: false was ignored")
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	src := replace(t, "  - name: movies\n    paths: [/mnt/movies]",
		"  - name: movies\n    profile: nope\n    paths: [relative/path]")
	_, err := Parse([]byte(src))
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("got %T, want *config.Error", err)
	}
	if len(ce.Problems) < 2 {
		t.Fatalf("expected both the bad profile and the relative path, got: %v", ce.Problems)
	}
}

func TestCheckPathsFindsMissingDirectories(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "movies")
	if err := os.Mkdir(media, 0o755); err != nil {
		t.Fatal(err)
	}

	src := strings.NewReplacer(
		"[/mnt/movies]", "["+media+"]",
		"work_dir: /temp", "work_dir: "+dir,
		"database: /config/db.sqlite", "database: "+filepath.Join(dir, "state.db"),
	).Replace(minimal)

	c := mustParse(t, src)
	if err := c.CheckPaths(); err != nil {
		t.Fatalf("paths that exist were reported as missing: %v", err)
	}

	// A missing mount is the realistic failure: the container starts before
	// the share is available and every library looks empty.
	src2 := strings.Replace(src, "["+media+"]", "["+filepath.Join(dir, "gone")+"]", 1)
	c2 := mustParse(t, src2)
	if err := c2.CheckPaths(); err == nil {
		t.Fatal("a missing library path was not reported")
	}
}

// Parsing must not need the filesystem: validating a server's config from a
// laptop is the normal way to check a change before deploying it.
func TestParseDoesNotTouchTheFilesystem(t *testing.T) {
	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("minimal config references /mnt/movies, which does not exist here: %v", err)
	}
}

func TestUnusedProfileIsAWarningNotAnError(t *testing.T) {
	src := replace(t, "  standard:\n", "  spare:\n    extends: standard\n  standard:\n")
	c := mustParse(t, src)
	if !hasWarning(c, "spare") {
		t.Errorf("expected a warning about the unused profile, got %v", c.Warnings)
	}
}

func TestForcingMP4Warns(t *testing.T) {
	src := replace(t, "  container: mkv", "  container: mp4")
	c := mustParse(t, src)
	if !hasWarning(c, "forces mp4") {
		t.Errorf("forcing mp4 drops subtitle tracks and renames files; it should warn. got %v", c.Warnings)
	}
}

func hasWarning(c *Config, substr string) bool {
	for _, w := range c.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// replaceAll applies a series of old/new pairs to a config source.
func replaceAll(t *testing.T, src string, pairs ...string) string {
	t.Helper()
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(src, pairs[i]) {
			t.Fatalf("test anchor %q is not in the config", pairs[i])
		}
		src = strings.Replace(src, pairs[i], pairs[i+1], 1)
	}
	return src
}
