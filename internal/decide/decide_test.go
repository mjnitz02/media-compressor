package decide_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/probe"
)

// build makes a probe result from a compact description, so a test can say
// what matters and stay readable.
type spec struct {
	path     string
	sizeMiB  float64
	duration float64
	streams  []probe.Stream
}

func build(s spec) *probe.Result {
	r := &probe.Result{Path: s.path, Streams: s.streams}
	r.Format.Duration = ftoa(s.duration)
	r.Format.Size = itoa(int64(s.sizeMiB * 1024 * 1024))
	return r
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func itoa(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func video(codec string) probe.Stream {
	return probe.Stream{Index: 0, CodecName: codec, CodecType: probe.TypeVideo,
		Width: 1920, Height: 1080, Disposition: map[string]int{}}
}

func audio(idx int, codec string, channels int, lang, title string) probe.Stream {
	tags := map[string]string{}
	if lang != "" {
		tags["language"] = lang
	}
	if title != "" {
		tags["title"] = title
	}
	return probe.Stream{Index: idx, CodecName: codec, CodecType: probe.TypeAudio,
		Channels: channels, Tags: tags, Disposition: map[string]int{}}
}

func subtitle(idx int, codec, lang, title string, forced bool) probe.Stream {
	tags := map[string]string{}
	if lang != "" {
		tags["language"] = lang
	}
	if title != "" {
		tags["title"] = title
	}
	d := map[string]int{}
	if forced {
		d["forced"] = 1
	}
	return probe.Stream{Index: idx, CodecName: codec, CodecType: probe.TypeSubtitle,
		Tags: tags, Disposition: d}
}

func args(p decide.Plan) string { return strings.Join(p.OutputArgs, " ") }

// --- the encode / skip decision -------------------------------------------

func TestAlreadyTargetCodecIsLeftAlone(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{video("hevc"), audio(1, "ac3", 6, "eng", "")}})

	plan := decide.Decide(r, decide.Standard())

	if plan.Video != decide.VideoCopy {
		t.Errorf("video = %q, want copy", plan.Video)
	}
	if plan.Action != decide.ActionNone {
		t.Errorf("action = %q, want none -- nothing about this file needs changing", plan.Action)
	}
}

// AV1 is more efficient than HEVC, so re-encoding it to HEVC spends time to
// lose quality. The old stack would have done it; this must not.
func TestAV1IsNeverDowngradedToHEVC(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 8000, duration: 3600,
		streams: []probe.Stream{video("av1"), audio(1, "aac", 2, "eng", "")}})

	if plan := decide.Decide(r, decide.Standard()); plan.Video != decide.VideoCopy {
		t.Errorf("Standard re-encodes AV1: video = %q, want copy", plan.Video)
	}
	// The compat profile is expected to disagree; that is what it is for.
	if plan := decide.Decide(r, decide.TdarrCompat()); plan.Video != decide.VideoEncode {
		t.Errorf("TdarrCompat should reproduce the AV1 downgrade, got %q", plan.Video)
	}
}

func TestBitrateFloorDeclinesTheEncode(t *testing.T) {
	// 700 MiB over 45 minutes is about 2170 kbps, well under the 3000 floor.
	r := build(spec{path: "/m/a.mkv", sizeMiB: 700, duration: 2700,
		streams: []probe.Stream{video("h264"), audio(1, "aac", 2, "eng", "")}})

	plan := decide.Decide(r, decide.Standard())

	if plan.Video != decide.VideoSkippedFloor {
		t.Fatalf("video = %q, want skipped_floor", plan.Video)
	}
	if !strings.Contains(plan.Reason, "below floor") {
		t.Errorf("Reason = %q, want it to explain the floor", plan.Reason)
	}
	// Declining the encode must not mean declining the cleanup, but it must
	// mean the video is copied.
	if strings.Contains(args(plan), "hevc") {
		t.Errorf("skipped file still has an encoder in its args: %s", args(plan))
	}
}

func TestEncodeProducesTheExpectedRateControl(t *testing.T) {
	// 1000 MiB over 1400s -> about 5992 kbps, so divisor 1.5.
	r := build(spec{path: "/m/a.mkv", sizeMiB: 1000, duration: 1400,
		streams: []probe.Stream{video("h264"), audio(1, "aac", 2, "eng", "")}})

	plan := decide.Decide(r, decide.Standard())

	if plan.Video != decide.VideoEncode {
		t.Fatalf("video = %q, want encode (reason: %s)", plan.Video, plan.Reason)
	}
	b := plan.Bitrate
	if b.TargetKbps != b.SourceKbps*2/3 && b.TargetKbps != int(float64(b.SourceKbps)/1.5) {
		t.Errorf("target %d is not the source %d cut by 1.5", b.TargetKbps, b.SourceKbps)
	}
	if b.MinKbps >= b.TargetKbps || b.MaxKbps <= b.TargetKbps {
		t.Errorf("min/max %d/%d do not bracket target %d", b.MinKbps, b.MaxKbps, b.TargetKbps)
	}
	if b.BufsizeKbps != b.SourceKbps {
		t.Errorf("bufsize = %d, want the full source bitrate %d", b.BufsizeKbps, b.SourceKbps)
	}
	for _, want := range []string{"-c:v hevc_vaapi", "-b:v", "-minrate", "-maxrate", "-bufsize"} {
		if !strings.Contains(args(plan), want) {
			t.Errorf("args missing %q: %s", want, args(plan))
		}
	}
	if got := strings.Join(plan.InputArgs, " "); !strings.Contains(got, "-hwaccel vaapi") {
		t.Errorf("hardware encoder without hwaccel input args: %q", got)
	}
}

func TestSoftwareEncoderGetsNoHardwareInputArgs(t *testing.T) {
	p := decide.Standard()
	p.Video.Encoder = "libx265"

	r := build(spec{path: "/m/a.mkv", sizeMiB: 1000, duration: 1400,
		streams: []probe.Stream{video("h264")}})

	plan := decide.Decide(r, p)
	if len(plan.InputArgs) != 0 {
		t.Errorf("libx265 got input args %q, want none", plan.InputArgs)
	}
}

func TestFileWithNoVideoIsAnError(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 50, duration: 600,
		streams: []probe.Stream{audio(0, "flac", 2, "eng", "")}})

	plan := decide.Decide(r, decide.Standard())
	if plan.Action != decide.ActionError {
		t.Errorf("action = %q, want error", plan.Action)
	}
}

// A file whose duration ffprobe could not determine must not be encoded on the
// strength of a divide-by-zero.
func TestZeroDurationNeverEncodes(t *testing.T) {
	r := &probe.Result{Path: "/m/a.mkv", Streams: []probe.Stream{video("h264")}}
	r.Format.Size = "5000000000"
	r.Format.Duration = "0"

	plan := decide.Decide(r, decide.Standard())
	if plan.Video == decide.VideoEncode {
		t.Error("encoded a file with unknown duration")
	}
	if !strings.Contains(plan.Reason, "bitrate") {
		t.Errorf("Reason = %q, want an explanation", plan.Reason)
	}
}

// --- track selection -------------------------------------------------------

func TestCommentaryTracksAreDropped(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "ac3", 6, "eng", "Surround 5.1"),
			audio(2, "ac3", 2, "eng", "Director's Commentary"),
			subtitle(3, "subrip", "eng", "", false),
			subtitle(4, "subrip", "eng", "SDH", false),
		}})

	plan := decide.Decide(r, decide.Standard())

	if !strings.Contains(args(plan), "-map -0:a:1") {
		t.Errorf("commentary audio not dropped: %s", args(plan))
	}
	if !strings.Contains(args(plan), "-map -0:s:1") {
		t.Errorf("SDH subtitle not dropped: %s", args(plan))
	}
	if strings.Contains(args(plan), "-map -0:a:0") {
		t.Errorf("dropped the main audio track: %s", args(plan))
	}
}

func TestUnwantedLanguagesAreDropped(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "ac3", 6, "eng", ""),
			audio(2, "ac3", 6, "rus", ""),
			subtitle(3, "subrip", "eng", "", false),
			subtitle(4, "subrip", "chi", "", false),
		}})

	plan := decide.Decide(r, decide.Standard())

	if !strings.Contains(args(plan), "-map -0:a:1") {
		t.Errorf("Russian audio not dropped: %s", args(plan))
	}
	if !strings.Contains(args(plan), "-map -0:s:1") {
		t.Errorf("Chinese subtitle not dropped: %s", args(plan))
	}
}

// Every profile keeps untagged tracks. An untagged track is often the one you
// actually want, and dropping audio is unrecoverable.
func TestUntaggedTracksAreKept(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "ac3", 6, "", ""),
			subtitle(2, "subrip", "", "", false),
		}})

	plan := decide.Decide(r, decide.Standard())
	if strings.Contains(args(plan), "-map -0:a:0") || strings.Contains(args(plan), "-map -0:s:0") {
		t.Errorf("dropped an untagged track: %s", args(plan))
	}
}

// A keep-list applied literally can leave a file with no audio at all. The
// realistic way in is a title match: a file whose only audio track is labelled
// as commentary. A commentary track is still infinitely better than silence.
func TestNeverLeavesAFileSilent(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "eac3", 6, "eng", "Director's Commentary"),
		}})

	plan := decide.Decide(r, decide.Standard())

	if strings.Contains(args(plan), "-map -0:a:0") {
		t.Fatalf("dropped the only audio track, leaving a silent file: %s", args(plan))
	}
	if len(plan.Notes) == 0 {
		t.Error("rescued the track but recorded no note explaining why")
	}
	// It still gets normalised like any other kept multichannel track.
	if !strings.Contains(args(plan), "-c:a:0 ac3") {
		t.Errorf("rescued track not normalised: %s", args(plan))
	}
}

// The guard is a floor, not a licence to keep everything: when something does
// survive the rules, the unwanted tracks still go.
func TestGuardDoesNotRescueWhenAnotherTrackSurvives(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "eac3", 6, "chi", ""),
			audio(2, "ac3", 6, "eng", ""),
		}})

	plan := decide.Decide(r, decide.Standard())

	if !strings.Contains(args(plan), "-map -0:a:0") {
		t.Errorf("Chinese track kept even though the English one survives: %s", args(plan))
	}
	if len(plan.Notes) != 0 {
		t.Errorf("recorded a rescue note when no rescue was needed: %v", plan.Notes)
	}
}

// The rescued track is the one a player would have chosen.
func TestGuardPrefersTheDefaultTrack(t *testing.T) {
	second := audio(2, "ac3", 2, "eng", "Commentary with the cast")
	second.Disposition["default"] = 1

	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "eac3", 6, "eng", "Director Commentary"),
			second,
		}})

	plan := decide.Decide(r, decide.Standard())

	if !strings.Contains(args(plan), "-map -0:a:0") {
		t.Errorf("non-default commentary track was kept: %s", args(plan))
	}
	if strings.Contains(args(plan), "-map -0:a:1") {
		t.Errorf("dropped the default track instead of rescuing it: %s", args(plan))
	}
}

// --- trusting the tags ----------------------------------------------------

// An anime release turning up with a Korean track and no Japanese one is much
// more likely to be mistagged than to genuinely contain nothing wanted. In
// that case nothing is pruned on language at all.
func TestTagsAreNotPrunedOnWhenNoExpectedLanguageIsPresent(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "aac", 2, "kor", ""),
			audio(2, "aac", 2, "rus", ""),
			subtitle(3, "ass", "kor", "", false),
			subtitle(4, "ass", "rus", "", false),
		}})

	plan := decide.Decide(r, decide.Anime())

	for _, drop := range []string{"-map -0:a:0", "-map -0:a:1", "-map -0:s:0", "-map -0:s:1"} {
		if strings.Contains(args(plan), drop) {
			t.Errorf("pruned %s on tags that are not credible: %s", drop, args(plan))
		}
	}
	if len(plan.Notes) == 0 {
		t.Error("kept everything but recorded no note saying why")
	}
}

// Once an expected language does appear, the tags look credible and the
// genuine extras are pruned as normal.
func TestExtrasArePrunedOnceTagsLookCredible(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "aac", 2, "jpn", ""),
			audio(2, "aac", 2, "rus", ""),
			audio(3, "aac", 2, "kor", ""),
		}})

	plan := decide.Decide(r, decide.Anime())

	if !strings.Contains(args(plan), "-map -0:a:1") {
		t.Errorf("Russian track kept despite a credible Japanese track: %s", args(plan))
	}
	// Korean is on the anime keep-list on purpose; see docs/libraries.md.
	if strings.Contains(args(plan), "-map -0:a:2") {
		t.Errorf("dropped a Korean track, which the anime profile keeps: %s", args(plan))
	}
}

// "und" is an explicit statement that the language is unknown, so it is not
// evidence that the tags are good.
func TestUndTagsAreNotEvidenceOfCredibleTagging(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "aac", 2, "und", ""),
			audio(2, "aac", 2, "rus", ""),
		}})

	plan := decide.Decide(r, decide.Anime())
	if strings.Contains(args(plan), "-map -0:a:1") {
		t.Errorf("pruned on tags where the only recognised value is und: %s", args(plan))
	}
}

// --- language tagging -----------------------------------------------------

// Untagged tracks get labelled so a player's track picker shows something
// better than "Unknown".
func TestUntaggedTracksAreLabelled(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "aac", 2, "eng", ""),
			audio(2, "aac", 2, "", ""),
			subtitle(3, "subrip", "", "", false),
		}})

	plan := decide.Decide(r, decide.Standard())

	if !strings.Contains(args(plan), "-metadata:s:a:1 language=eng") {
		t.Errorf("untagged audio not labelled: %s", args(plan))
	}
	if !strings.Contains(args(plan), "-metadata:s:s:0 language=eng") {
		t.Errorf("untagged subtitle not labelled: %s", args(plan))
	}
	if strings.Contains(args(plan), "-metadata:s:a:0") {
		t.Errorf("relabelled a track that already had a language: %s", args(plan))
	}
}

// Anime assumes untagged audio is Japanese, which is far more often right than
// not for that library.
func TestAnimeLabelsUntaggedAudioAsJapanese(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{video("hevc"), audio(1, "aac", 2, "", "")}})

	plan := decide.Decide(r, decide.Anime())
	if !strings.Contains(args(plan), "-metadata:s:a:0 language=jpn") {
		t.Errorf("untagged anime audio not labelled jpn: %s", args(plan))
	}
}

// A track on its way out does not need a language tag.
func TestDroppedTracksAreNotLabelled(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "aac", 2, "eng", ""),
			audio(2, "aac", 2, "", "Commentary"),
		}})

	plan := decide.Decide(r, decide.Standard())
	if strings.Contains(args(plan), "-metadata:s:a:1") {
		t.Errorf("labelled a track that is being dropped: %s", args(plan))
	}
}

func TestTaggingCanBeTurnedOff(t *testing.T) {
	p := decide.Standard()
	p.Audio.TagUntaggedAs = ""
	p.Subtitles.TagUntaggedAs = ""

	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{video("hevc"), audio(1, "aac", 2, "", "")}})

	plan := decide.Decide(r, p)
	if strings.Contains(args(plan), "-metadata") {
		t.Errorf("tagging disabled but metadata args present: %s", args(plan))
	}
	if plan.Action != decide.ActionNone {
		t.Errorf("action = %q, want none once tagging is off", plan.Action)
	}
}

// Forced subtitles are the translated-signage track for a film watched in its
// original audio. Losing one is very visible, so they outrank the language
// list.
func TestForcedSubtitlesSurviveTheLanguageFilter(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "ac3", 6, "jpn", ""),
			subtitle(2, "subrip", "spa", "", true),
		}})

	if plan := decide.Decide(r, decide.Standard()); strings.Contains(args(plan), "-map -0:s:0") {
		t.Errorf("Standard dropped a forced subtitle: %s", args(plan))
	}
	// The old stack ignored the forced flag.
	if plan := decide.Decide(r, decide.TdarrCompat()); !strings.Contains(args(plan), "-map -0:s:0") {
		t.Errorf("TdarrCompat should drop it, got: %s", args(plan))
	}
}

func TestAnimeKeepsSDHSubtitles(t *testing.T) {
	// On an anime release the SDH track is frequently the only English one.
	r := build(spec{path: "/m/a.mkv", sizeMiB: 2000, duration: 1400,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "aac", 2, "jpn", ""),
			subtitle(2, "ass", "eng", "English (SDH)", false),
		}})

	if plan := decide.Decide(r, decide.Anime()); strings.Contains(args(plan), "-map -0:s:0") {
		t.Errorf("Anime dropped an SDH subtitle: %s", args(plan))
	}
	if plan := decide.Decide(r, decide.Standard()); !strings.Contains(args(plan), "-map -0:s:0") {
		t.Errorf("Standard should drop SDH, got: %s", args(plan))
	}
}

func TestMultichannelAudioIsNormalisedButStereoIsNot(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "dts", 6, "eng", ""),
			audio(2, "aac", 2, "eng", ""),
			audio(3, "ac3", 6, "eng", ""),
		}})

	plan := decide.Decide(r, decide.Standard())

	if !strings.Contains(args(plan), "-c:a:0 ac3") {
		t.Errorf("6-channel DTS not converted: %s", args(plan))
	}
	if strings.Contains(args(plan), "-c:a:1") {
		t.Errorf("stereo AAC should be copied, not converted: %s", args(plan))
	}
	if strings.Contains(args(plan), "-c:a:2") {
		t.Errorf("6-channel AC-3 is already the target, should be copied: %s", args(plan))
	}
}

func TestCoverArtIsStrippedNotEncoded(t *testing.T) {
	// The exact shape of the twelve corpus files that the old stack routed
	// into a video encode because of an attached PNG.
	r := build(spec{path: "/m/a.mkv", sizeMiB: 8000, duration: 2700,
		streams: []probe.Stream{
			video("hevc"),
			{Index: 1, CodecName: "mjpeg", CodecType: probe.TypeVideo,
				Disposition: map[string]int{"attached_pic": 1}},
			{Index: 2, CodecName: "png", CodecType: probe.TypeVideo,
				Disposition: map[string]int{"attached_pic": 1}},
			audio(3, "ac3", 6, "eng", ""),
		}})

	plan := decide.Decide(r, decide.Standard())

	if plan.Video != decide.VideoCopy {
		t.Errorf("video = %q, want copy -- artwork must never trigger an encode", plan.Video)
	}
	for _, want := range []string{"-map -0:v:1", "-map -0:v:2"} {
		if !strings.Contains(args(plan), want) {
			t.Errorf("args missing %q: %s", want, args(plan))
		}
	}
}

// --- the container toggle --------------------------------------------------

func TestMP4DropsImageSubtitlesAndConvertsTextOnes(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerMP4

	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "ac3", 6, "eng", ""),
			subtitle(2, "subrip", "eng", "", false),
			subtitle(3, "hdmv_pgs_subtitle", "eng", "", false),
		}})

	plan := decide.Decide(r, p)

	if plan.Container != decide.ContainerMP4 {
		t.Fatalf("container = %q, want mp4", plan.Container)
	}
	if !strings.Contains(args(plan), "-c:s:0 mov_text") {
		t.Errorf("SubRip not converted to mov_text: %s", args(plan))
	}
	if !strings.Contains(args(plan), "-map -0:s:1") {
		t.Errorf("PGS subtitle not dropped: %s", args(plan))
	}
	if !strings.Contains(args(plan), "-movflags +faststart") {
		t.Errorf("mp4 output missing faststart: %s", args(plan))
	}

	// The dropped PGS track must be reported with a reason a human can read,
	// because this is the toggle's real cost.
	var found bool
	for _, d := range plan.Drops {
		if d.Kind == "subtitle" && d.Codec == "hdmv_pgs_subtitle" {
			found = true
			if !strings.Contains(d.Reason, "mp4") {
				t.Errorf("drop reason = %q, want it to name the container", d.Reason)
			}
		}
	}
	if !found {
		t.Error("PGS drop not recorded in plan.Drops")
	}
}

func TestMP4ConvertsAudioItCannotCarry(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerMP4

	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "truehd", 8, "eng", ""),
			audio(2, "flac", 2, "eng", ""),
			audio(3, "aac", 2, "eng", ""),
		}})

	plan := decide.Decide(r, p)

	if !strings.Contains(args(plan), "-c:a:0 ac3") {
		t.Errorf("multichannel TrueHD not converted to ac3: %s", args(plan))
	}
	if !strings.Contains(args(plan), "-c:a:1 aac") {
		t.Errorf("stereo FLAC not converted to aac: %s", args(plan))
	}
	if strings.Contains(args(plan), "-c:a:2") {
		t.Errorf("AAC is mp4-native and should be copied: %s", args(plan))
	}
}

func TestChangingContainerChangesOnlyTheExtension(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerMP4

	r := build(spec{path: "/mnt/media/movies/Example (2019)/Example.2019.mkv",
		sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{video("hevc"), audio(1, "aac", 2, "eng", "")}})

	plan := decide.Decide(r, p)

	if plan.Action != decide.ActionRemux {
		t.Errorf("action = %q, want remux -- the container has to change", plan.Action)
	}
	if !plan.ExtensionChanges() {
		t.Error("ExtensionChanges() = false, but mkv -> mp4 renames the file")
	}
	if plan.SourceContainer != "mkv" {
		t.Errorf("SourceContainer = %q, want mkv", plan.SourceContainer)
	}
	if got := plan.Container.Extension(); got != ".mp4" {
		t.Errorf("Extension() = %q, want .mp4", got)
	}
}

// ContainerSource is the conservative setting for a mixed library: it never
// remuxes a file just to change its container.
func TestContainerSourceKeepsWhatIsThere(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerSource

	for _, ext := range []string{"mkv", "mp4"} {
		r := build(spec{path: "/m/a." + ext, sizeMiB: 4000, duration: 3600,
			streams: []probe.Stream{video("hevc"), audio(1, "aac", 2, "eng", "")}})

		plan := decide.Decide(r, p)
		if string(plan.Container) != ext {
			t.Errorf("%s: container = %q, want %q", ext, plan.Container, ext)
		}
		if plan.ExtensionChanges() {
			t.Errorf("%s: renamed a file it had no reason to rename", ext)
		}
	}
}

// A container this tool does not write still has to land somewhere.
func TestContainerSourceFallsBackForExoticContainers(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerSource

	r := build(spec{path: "/m/old.avi", sizeMiB: 700, duration: 2700,
		streams: []probe.Stream{video("mpeg4"), audio(1, "mp3", 2, "eng", "")}})

	plan := decide.Decide(r, p)
	if plan.Container != decide.ContainerMKV {
		t.Errorf("container = %q, want mkv fallback for avi", plan.Container)
	}
}

func TestMKVRewritesMovTextAsSubRip(t *testing.T) {
	r := build(spec{path: "/m/a.mp4", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "aac", 2, "eng", ""),
			subtitle(2, "mov_text", "eng", "", false),
		}})

	plan := decide.Decide(r, decide.Standard())
	if !strings.Contains(args(plan), "-c:s:0 subrip") {
		t.Errorf("mov_text not rewritten for Matroska: %s", args(plan))
	}
}

// --- structural guarantees -------------------------------------------------

// decide must never express an opinion about where a file lives. If a path
// ever leaks into the arguments, the in-place-replacement guarantee is no
// longer enforceable by inspection.
func TestArgsNeverContainPaths(t *testing.T) {
	r := build(spec{path: "/mnt/media_isolated/movies/Secret.2019.mkv",
		sizeMiB: 1000, duration: 1400,
		streams: []probe.Stream{video("h264"), audio(1, "ac3", 6, "eng", "")}})

	plan := decide.Decide(r, decide.Standard())
	for _, a := range append(plan.InputArgs, plan.OutputArgs...) {
		if strings.Contains(a, "/mnt/") || strings.Contains(a, "Secret") {
			t.Errorf("argument %q leaks the file path", a)
		}
	}
}

// Mapping is subtractive: -map 0 takes everything, then negative maps remove
// specific streams. A stream type nobody anticipated is therefore kept rather
// than silently lost.
func TestMappingIsSubtractive(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{video("hevc"), audio(1, "ac3", 6, "eng", "")}})

	plan := decide.Decide(r, decide.Standard())
	if !strings.HasPrefix(args(plan), "-map 0 ") {
		t.Errorf("args do not start with -map 0: %s", args(plan))
	}
}

// --- bailing out is the normal outcome ------------------------------------

// This tool is an optimiser, and doing nothing is a perfectly good result. A
// stream type it does not recognise is kept, not guessed at: -map 0 takes
// everything and only specific, understood streams are mapped back out.
func TestUnrecognisedStreamTypesAreKept(t *testing.T) {
	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "ac3", 6, "eng", ""),
			// A timecode track, an unknown future codec_type, and a font.
			{Index: 2, CodecType: "timecode", CodecName: "smpte_2038", Disposition: map[string]int{}},
			{Index: 3, CodecType: "something_new", CodecName: "mystery", Disposition: map[string]int{}},
			{Index: 4, CodecType: probe.TypeAttach, CodecName: "ttf",
				Tags: map[string]string{"filename": "font.ttf"}, Disposition: map[string]int{}},
		}})

	plan := decide.Decide(r, decide.Standard())

	if plan.Action != decide.ActionNone {
		t.Errorf("action = %q, want none -- an unrecognised stream is not a reason to act", plan.Action)
	}
	for _, unexpected := range []string{"-0:2", "-0:3", "-0:4", "timecode", "mystery", "ttf"} {
		if strings.Contains(args(plan), unexpected) {
			t.Errorf("args touch an unrecognised stream (%q): %s", unexpected, args(plan))
		}
	}
}

// Every way out of the decision engine that leaves the file alone, in one
// place. None of these is a failure; all of them are the tool working.
func TestTheWaysItDeclinesToActAreAllReachable(t *testing.T) {
	cases := []struct {
		name    string
		streams []probe.Stream
		size    float64
		dur     float64
		want    decide.VideoDecision
		action  decide.Action
	}{
		{
			name:    "already the target codec",
			streams: []probe.Stream{video("hevc"), audio(1, "ac3", 6, "eng", "")},
			size:    4000, dur: 3600,
			want: decide.VideoCopy, action: decide.ActionNone,
		},
		{
			name:    "more efficient than the target codec",
			streams: []probe.Stream{video("av1"), audio(1, "ac3", 6, "eng", "")},
			size:    9000, dur: 3600,
			want: decide.VideoCopy, action: decide.ActionNone,
		},
		{
			name:    "bitrate too low to be worth cutting",
			streams: []probe.Stream{video("h264"), audio(1, "ac3", 6, "eng", "")},
			size:    700, dur: 2700,
			want: decide.VideoSkippedFloor, action: decide.ActionRemux,
		},
		{
			name:    "duration unknown, so the bitrate is unknowable",
			streams: []probe.Stream{video("h264"), audio(1, "ac3", 6, "eng", "")},
			size:    4000, dur: 0,
			want: decide.VideoSkippedFloor, action: decide.ActionRemux,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := build(spec{path: "/m/a.mkv", sizeMiB: tc.size, duration: tc.dur, streams: tc.streams})
			plan := decide.Decide(r, decide.Standard())

			if plan.Video != tc.want {
				t.Errorf("video = %q, want %q", plan.Video, tc.want)
			}
			if plan.Action == decide.ActionEncode {
				t.Errorf("action = encode, but this case should never re-encode")
			}
			if plan.Video != decide.VideoCopy && plan.Reason == "" {
				t.Error("declined without recording a reason")
			}
		})
	}
}

// --- keeping the container a file already has ------------------------------

// The case that motivated `source`: a library holding a mix of MKV and MP4,
// where a file turns out to be a viable encode candidate on bitrate. It should
// be re-encoded to HEVC and stay in whatever container it was already in,
// rather than being forced into MKV.
func TestSourceContainerEncodesWithoutChangingContainer(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerSource

	for _, ext := range []string{"mkv", "mp4"} {
		t.Run(ext, func(t *testing.T) {
			r := build(spec{path: "/m/show.s01e01." + ext, sizeMiB: 1000, duration: 1400,
				streams: []probe.Stream{video("h264"), audio(1, "aac", 2, "eng", "")}})

			plan := decide.Decide(r, p)

			if plan.Video != decide.VideoEncode {
				t.Fatalf("video = %q, want encode (reason: %s)", plan.Video, plan.Reason)
			}
			if string(plan.Container) != ext {
				t.Errorf("container = %q, want %q", plan.Container, ext)
			}
			if plan.ExtensionChanges() {
				t.Errorf("re-encoding renamed the file, which breaks in-place replacement")
			}
		})
	}
}

// A stream sitting in a container is proof the container can hold it, so a
// file keeping its own container must never have a codec change forced on it.
//
// The FLAC case is the one that bites: FLAC in MP4 is legal and plays, so
// "converting" it back into MP4 as AAC would discard quality for nothing.
func TestKeepingTheContainerForcesNothing(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerSource

	r := build(spec{path: "/m/a.mp4", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "flac", 2, "eng", ""),
			audio(2, "truehd", 8, "eng", ""),
			subtitle(3, "mov_text", "eng", "", false),
		}})

	plan := decide.Decide(r, p)

	if strings.Contains(args(plan), "-c:a:0") {
		t.Errorf("re-encoded FLAC that was already muxed in this container: %s", args(plan))
	}
	if strings.Contains(args(plan), "-c:s:0") {
		t.Errorf("rewrote a subtitle that was already muxed in this container: %s", args(plan))
	}
	for _, d := range plan.Drops {
		t.Errorf("dropped %s stream %d (%s) from a file that keeps its container: %s",
			d.Kind, d.TypeIdx, d.Codec, d.Reason)
	}

	// The 8-channel TrueHD is still normalised, but by the profile's own
	// multichannel rule rather than by the container.
	if !strings.Contains(args(plan), "-c:a:1 ac3") {
		t.Errorf("multichannel TrueHD not normalised: %s", args(plan))
	}
}

// Same guarantee for MKV: a PGS subtitle stays put, where forcing the file to
// MP4 would have to drop it.
func TestKeepingMKVRetainsImageSubtitles(t *testing.T) {
	p := decide.Standard()
	p.Container = decide.ContainerSource

	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600,
		streams: []probe.Stream{
			video("hevc"),
			audio(1, "ac3", 6, "eng", ""),
			subtitle(2, "hdmv_pgs_subtitle", "eng", "", false),
		}})

	plan := decide.Decide(r, p)
	if strings.Contains(args(plan), "-map -0:s:0") {
		t.Errorf("dropped a PGS subtitle from a file staying in MKV: %s", args(plan))
	}
}

// Forcing a container is the only thing that can cost a stream, so that is
// where the cost has to be visible.
func TestOnlyForcingAContainerCanCostAStream(t *testing.T) {
	streams := []probe.Stream{
		video("hevc"),
		audio(1, "ac3", 6, "eng", ""),
		subtitle(2, "hdmv_pgs_subtitle", "eng", "", false),
	}

	keep := decide.Standard()
	keep.Container = decide.ContainerSource
	force := decide.Standard()
	force.Container = decide.ContainerMP4

	r := build(spec{path: "/m/a.mkv", sizeMiB: 4000, duration: 3600, streams: streams})

	if plan := decide.Decide(r, keep); len(plan.Drops) != 0 {
		t.Errorf("keeping the container dropped %d streams", len(plan.Drops))
	}
	plan := decide.Decide(r, force)
	if len(plan.Drops) == 0 {
		t.Fatal("forcing mkv -> mp4 dropped nothing, but PGS cannot be muxed there")
	}
	if !plan.ExtensionChanges() {
		t.Error("ExtensionChanges() = false for a forced mkv -> mp4 conversion")
	}
}
