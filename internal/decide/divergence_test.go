package decide_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// TestStandardDivergesFromTdarrOnlyWhereIntended answers the question you
// actually want answered before pointing this at a real library: if I run the
// Standard profile instead of the stack I have been running for years, what
// changes?
//
// It runs both profiles over the same 1,881 real files and pins the
// difference. Every number below is a deliberate correction, and each one is
// asserted so that an unintended change to the decision engine shows up here
// as a failing count rather than as a surprise on the library.
func TestStandardDivergesFromTdarrOnlyWhereIntended(t *testing.T) {
	corpus := loadCorpus(t)

	old := decide.TdarrCompat()
	new := decide.Standard()

	var (
		sameDecision int
		videoChanges = map[string]int{}
		argsDiffer   int
		examples     = map[string]string{}

		// Dropping a track is the only irreversible thing in this project, so
		// the two directions are counted separately.
		moreAudioDropped int
		moreAudioKept    int
		moreSubsDropped  int
		moreSubsKept     int
	)

	for _, fx := range corpus {
		before := decide.Decide(&fx.Source.FFProbeData, old)
		after := decide.Decide(&fx.Source.FFProbeData, new)

		if before.Video == after.Video {
			sameDecision++
		} else {
			key := fmt.Sprintf("%s -> %s", before.Video, after.Video)
			videoChanges[key]++
			if _, seen := examples[key]; !seen {
				examples[key] = fmt.Sprintf("%s (%s, %s)",
					fx.Report, fx.Source.VideoCodecName, after.Reason)
			}
		}

		if strings.Join(before.OutputArgs, " ") != strings.Join(after.OutputArgs, " ") {
			argsDiffer++
		}

		switch b, a := dropsOfKind(before, "audio"), dropsOfKind(after, "audio"); {
		case a > b:
			moreAudioDropped++
		case a < b:
			moreAudioKept++
		}
		switch b, a := dropsOfKind(before, "subtitle"), dropsOfKind(after, "subtitle"); {
		case a > b:
			moreSubsDropped++
		case a < b:
			moreSubsKept++
		}
	}

	// Report first, assert second, so a failure comes with the full picture.
	t.Logf("video decision unchanged on %d of %d files", sameDecision, len(corpus))
	keys := make([]string, 0, len(videoChanges))
	for k := range videoChanges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("  %-32s %4d   e.g. %s", k, videoChanges[k], examples[k])
	}
	t.Logf("ffmpeg arguments differ on %d of %d files", argsDiffer, len(corpus))
	t.Logf("audio tracks:    %d files drop more, %d files keep more", moreAudioDropped, moreAudioKept)
	t.Logf("subtitle tracks: %d files drop more, %d files keep more", moreSubsDropped, moreSubsKept)

	// 28 AV1 files stop being encode candidates. The old stack would have
	// transcoded AV1 down to HEVC -- a straight quality and efficiency loss --
	// and was saved from it only by the bitrate floor. Under Standard they are
	// left alone on purpose rather than by luck.
	if got := videoChanges["skipped_floor -> copy"]; got != 40 {
		t.Errorf("skipped_floor -> copy = %d, want 40 (28 AV1 + 12 PNG-cover-art files)", got)
	}

	// Nothing should newly become an encode. Standard is strictly more
	// conservative about what it will re-encode than the stack it replaces.
	for _, k := range keys {
		if strings.HasSuffix(k, "-> encode") {
			t.Errorf("Standard newly encodes %d files (%s); it must never be "+
				"less conservative than the old stack", videoChanges[k], k)
		}
	}

	// The remaining differences are all track selection and cover art, which
	// change the ffmpeg command but not whether a video encode happens.
	if argsDiffer == 0 {
		t.Error("no argument differences at all -- the corrections are not taking effect")
	}

	// Standard folds in the audio language filtering that used to be a
	// separate plugin, so in principle it could drop tracks the old encoder
	// plugin kept. It must not: dropping audio is the one irreversible thing
	// this tool does.
	//
	// Getting to zero here needed the last-track guard in buildAudio. Without
	// it, two files in this corpus -- single audio tracks tagged chi and aze --
	// came out as silent video.
	if moreAudioDropped != 0 {
		t.Errorf("%d files would lose an audio track the old stack kept; "+
			"dropping audio is unrecoverable and this must stay at zero",
			moreAudioDropped)
	}

	// Keeping forced subtitles is the one place Standard is deliberately more
	// generous.
	if moreSubsKept == 0 {
		t.Error("no file keeps more subtitles; the forced-subtitle rule is not taking effect")
	}
}

// dropsOfKind counts the streams of one kind a plan removes.
func dropsOfKind(p decide.Plan, kind string) int {
	n := 0
	for _, d := range p.Drops {
		if d.Kind == kind {
			n++
		}
	}
	return n
}
