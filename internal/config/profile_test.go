package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// profileDiff compares two profiles section by section, so a failure names
// the part of the YAML that is wrong rather than dumping two large structs.
func profileDiff(got, want decide.Profile) string {
	sections := []struct {
		name      string
		got, want any
	}{
		{"name", got.Name, want.Name},
		{"container", got.Container, want.Container},
		{"video", got.Video, want.Video},
		{"audio", got.Audio, want.Audio},
		{"subtitles", got.Subtitles, want.Subtitles},
		{"strip", got.Strip, want.Strip},
		{"quirks", got.Quirks, want.Quirks},
	}
	var b strings.Builder
	for _, s := range sections {
		if !reflect.DeepEqual(s.got, s.want) {
			fmt.Fprintf(&b, "  %s:\n    got  %+v\n    want %+v\n", s.name, s.got, s.want)
		}
	}
	return b.String()
}

// extends is the reason this project exists: a folder wanting slightly
// different treatment should be a few lines, not a duplicated stack.
func TestExtendsOverridesOnlyWhatItNames(t *testing.T) {
	src := replace(t, "libraries:", `  gentle:
    extends: standard
    audio:
      keep_languages: [eng, jpn]
libraries:`)
	c := mustParse(t, src)

	base, gentle := c.Profiles["standard"], c.Profiles["gentle"]
	if got, want := gentle.Audio.KeepLanguages, []string{"eng", "jpn"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keep_languages = %v, want %v", got, want)
	}
	// A sibling field inside the same `audio:` block must survive.
	if gentle.Audio.MultichannelTo != base.Audio.MultichannelTo {
		t.Errorf("multichannel_to = %q, want the inherited %q", gentle.Audio.MultichannelTo, base.Audio.MultichannelTo)
	}
	// And so must an entire block the child never mentions.
	if !reflect.DeepEqual(gentle.Video, base.Video) {
		t.Errorf("video block was not inherited:\n got %+v\nwant %+v", gentle.Video, base.Video)
	}
	if gentle.Name != "gentle" {
		t.Errorf("name = %q, want gentle", gentle.Name)
	}
}

// A keep-list in a child replaces the parent's rather than adding to it.
// "keep_languages: [eng]" has to mean "only English", or a profile could
// never narrow anything.
func TestExtendsReplacesListsRatherThanAppending(t *testing.T) {
	src := replace(t, "      keep_languages: [eng]\n      primary_languages: [eng]",
		"      keep_languages: [eng, jpn, kor]\n      primary_languages: [eng]")
	src = strings.Replace(src, "libraries:", `  narrow:
    extends: standard
    audio:
      keep_languages: [eng]
libraries:`, 1)

	c := mustParse(t, src)
	if got, want := c.Profiles["narrow"].Audio.KeepLanguages, []string{"eng"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keep_languages = %v, want %v (lists replace, they do not merge)", got, want)
	}
}

func TestExtendsChainsThroughSeveralProfiles(t *testing.T) {
	src := replace(t, "libraries:", `  middle:
    extends: standard
    audio:
      multichannel_to: eac3
  leaf:
    extends: middle
    subtitles:
      keep_languages: [eng, jpn]
libraries:`)
	c := mustParse(t, src)

	leaf := c.Profiles["leaf"]
	if leaf.Audio.MultichannelTo != "eac3" {
		t.Errorf("multichannel_to = %q, want eac3 inherited through middle", leaf.Audio.MultichannelTo)
	}
	if leaf.Video.Encoder != "libx265" {
		t.Errorf("encoder = %q, want libx265 inherited from standard", leaf.Video.Encoder)
	}
}

func TestExtendsCycleIsReportedRatherThanHanging(t *testing.T) {
	src := replace(t, "libraries:", `  a:
    extends: b
  b:
    extends: a
libraries:`)
	wantProblem(t, src, "cycle")
}

func TestExtendsUnknownParentIsAnError(t *testing.T) {
	src := replace(t, "libraries:", `  odd:
    extends: nosuchprofile
libraries:`)
	wantProblem(t, src, "nosuchprofile")
}

// Container precedence is library, then profile, then file defaults. It is
// worth a test because the setting decides whether files get renamed.
func TestContainerPrecedence(t *testing.T) {
	src := replace(t, "libraries:", `  keeper:
    extends: standard
    container: source
libraries:`)
	src = strings.Replace(src, `  - name: movies
    paths: [/mnt/movies]`, `  - name: movies
    paths: [/mnt/movies]
  - name: mixed
    profile: keeper
    paths: [/mnt/mixed]
  - name: forced
    profile: keeper
    container: mp4
    paths: [/mnt/forced]`, 1)

	c := mustParse(t, src)
	want := map[string]decide.Container{
		"movies": decide.ContainerMKV,    // from defaults
		"mixed":  decide.ContainerSource, // from the profile
		"forced": decide.ContainerMP4,    // from the library
	}
	for _, lib := range c.Libraries {
		if got := lib.Profile.Container; got != want[lib.Name] {
			t.Errorf("library %s: container = %q, want %q", lib.Name, got, want[lib.Name])
		}
	}
	// The shared profile itself must not have been mutated by the library
	// that overrode it.
	if c.Profiles["keeper"].Container != decide.ContainerSource {
		t.Errorf("the keeper profile was mutated by a library override: %q", c.Profiles["keeper"].Container)
	}
}

// "When in doubt, keep it" is the rule these three booleans encode, so an
// omitted key has to mean keep.
func TestKeepFlagsDefaultToKeeping(t *testing.T) {
	c := mustParse(t, minimal)
	p := c.Profiles["standard"]
	if !p.Audio.KeepUntagged {
		t.Error("audio.keep_untagged should default to true")
	}
	if !p.Subtitles.KeepUntagged {
		t.Error("subtitles.keep_untagged should default to true")
	}
	if !p.Subtitles.KeepForced {
		t.Error("subtitles.keep_forced should default to true; a forced track is the signage track")
	}

	src := replace(t, "    subtitles:\n      keep_languages: [eng]",
		"    subtitles:\n      keep_languages: [eng]\n      keep_forced: false\n      keep_untagged: false")
	off := mustParse(t, src).Profiles["standard"]
	if off.Subtitles.KeepForced || off.Subtitles.KeepUntagged {
		t.Error("an explicit false must still be honoured")
	}
}

func TestSourceBitrateBasisDefaultsToContainer(t *testing.T) {
	c := mustParse(t, minimal)
	if got := c.Profiles["standard"].Video.Bitrate.Basis; got != decide.BasisContainer {
		t.Errorf("basis = %q, want container (the basis the old stack used)", got)
	}

	src := replace(t, "      target_codec: hevc", "      target_codec: hevc\n      source_bitrate_basis: video_stream")
	if got := mustParse(t, src).Profiles["standard"].Video.Bitrate.Basis; got != decide.BasisVideoStream {
		t.Errorf("basis = %q, want video_stream", got)
	}
}
