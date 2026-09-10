package decide_test

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/probe"
)

// This is the test the project exists to pass.
//
// testdata/tdarr_decisions.jsonl.gz holds 1,881 real (probe -> decision) pairs
// recorded from the Tdarr stack being replaced. Matching all of them is how we
// know the output quality carries over, rather than hoping it does. Once this
// is green, any deliberate quality change becomes a reviewed diff here instead
// of a leap of faith.
//
// It asserts three things per fixture:
//
//  1. the same video decision (encode / declined at the floor / leave alone),
//  2. the same bitrate arithmetic, to the kbps, and
//  3. the same ffmpeg arguments, token for token.
//
// The third is the strict one, and it is why decide.Profile carries a Quirks
// struct: the old stack had genuine bugs, and reproducing its commands means
// reproducing those too. See decide.TdarrCompat.

const corpusPath = "../../testdata/tdarr_decisions.jsonl.gz"

type fixture struct {
	Report string `json:"report"`
	Kind   string `json:"kind"`
	Source struct {
		Container      string       `json:"container"`
		VideoCodecName string       `json:"video_codec_name"`
		FileSize       float64      `json:"file_size"`
		FFProbeData    probe.Result `json:"ffProbeData"`
	} `json:"source"`
	Expected struct {
		InfoLog string `json:"infoLog"`
		CLI     string `json:"cli"`
		Acted   bool   `json:"acted"`
	} `json:"expected"`
}

func loadCorpus(t *testing.T) []fixture {
	t.Helper()

	f, err := os.Open(filepath.Clean(corpusPath))
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip corpus: %v", err)
	}
	defer gz.Close()

	var out []fixture
	dec := json.NewDecoder(gz)
	for dec.More() {
		var fx fixture
		if err := dec.Decode(&fx); err != nil {
			t.Fatalf("decode fixture %d: %v", len(out)+1, err)
		}
		// The corpus records the probe but not the path. decide only reads the
		// path to get the container from the extension, so a synthetic one in
		// the right container is enough -- and it keeps real library paths out
		// of the repository.
		fx.Source.FFProbeData.Path = "/library/fixture." + fx.Source.Container
		out = append(out, fx)
	}
	return out
}

func TestGoldenCorpus(t *testing.T) {
	corpus := loadCorpus(t)
	if len(corpus) != 1881 {
		t.Fatalf("corpus has %d fixtures, expected 1881", len(corpus))
	}

	profile := decide.TdarrCompat()

	var checkedArgs, checkedBitrate int
	counts := map[string]int{}

	for _, fx := range corpus {
		counts[fx.Kind]++

		plan := decide.Decide(&fx.Source.FFProbeData, profile)

		// 1. The video decision.
		want, ok := expectedVideoDecision(fx.Kind)
		if !ok {
			t.Fatalf("%s: corpus has unexpected kind %q", fx.Report, fx.Kind)
		}
		if plan.Video != want {
			t.Errorf("%s (%s): video decision = %q, want %q\n  reason: %s",
				fx.Report, fx.Kind, plan.Video, want, plan.Reason)
			continue
		}

		// 2. The bitrate arithmetic, wherever the log recorded numbers.
		if want, ok := parseBitrateLog(fx.Expected.InfoLog); ok {
			checkedBitrate++
			if plan.Bitrate != want {
				t.Errorf("%s: bitrate = %+v, want %+v", fx.Report, plan.Bitrate, want)
			}
		}

		// 3. The ffmpeg arguments. Four of the 1,881 fixtures recorded no
		// attributable command, because another plugin in the chain was the
		// one that acted that cycle.
		if fx.Expected.CLI == "" {
			continue
		}
		wantIn, wantOut, err := parseRecordedCLI(fx.Expected.CLI)
		if err != nil {
			t.Fatalf("%s: %v", fx.Report, err)
		}
		checkedArgs++
		if got := strings.Join(plan.OutputArgs, " "); got != strings.Join(wantOut, " ") {
			t.Errorf("%s: output args mismatch\n  got:  %s\n  want: %s",
				fx.Report, got, strings.Join(wantOut, " "))
		}
		if got := strings.Join(plan.InputArgs, " "); got != strings.Join(wantIn, " ") {
			t.Errorf("%s: input args mismatch\n  got:  %q\n  want: %q",
				fx.Report, got, strings.Join(wantIn, " "))
		}
	}

	t.Logf("corpus composition: %v", counts)
	t.Logf("asserted %d ffmpeg command lines and %d bitrate calculations",
		checkedArgs, checkedBitrate)
}

// expectedVideoDecision maps the corpus's recorded classification onto this
// project's vocabulary.
func expectedVideoDecision(kind string) (decide.VideoDecision, bool) {
	switch kind {
	case "transcode":
		return decide.VideoEncode, true
	case "skip_bitrate_floor":
		return decide.VideoSkippedFloor, true
	case "already_done":
		return decide.VideoCopy, true
	}
	return "", false
}

var bitrateLogRe = regexp.MustCompile(`• (Original|Target|Minimum|Maximum) Bitrate: (\d+)`)

// parseBitrateLog pulls the four numbers the old plugin printed when it
// encoded. Skipped files log only the target, which is checked separately by
// the decision comparison.
func parseBitrateLog(infoLog string) (decide.Bitrate, bool) {
	m := bitrateLogRe.FindAllStringSubmatch(infoLog, -1)
	if len(m) != 4 {
		return decide.Bitrate{}, false
	}
	var b decide.Bitrate
	for _, g := range m {
		n, err := strconv.Atoi(g[2])
		if err != nil {
			return decide.Bitrate{}, false
		}
		switch g[1] {
		case "Original":
			b.SourceKbps = n
			b.BufsizeKbps = n
		case "Target":
			b.TargetKbps = n
		case "Minimum":
			b.MinKbps = n
		case "Maximum":
			b.MaxKbps = n
		}
	}
	return b, true
}

const (
	// Every recorded command starts its output arguments with these, because
	// the plugin's defaults were `-map 0`, `-map -0:d`, `-c:v copy`.
	outputArgsStart = "-map 0 -map -0:d"
	// ...and ends with this.
	outputArgsEnd = "-max_muxing_queue_size 4096"
)

// parseRecordedCLI splits a recorded command into the arguments before -i and
// the arguments after the input path, discarding both paths.
//
// Splitting on whitespace is not an option: the recorded commands contain real
// library paths with spaces, some quoted and some not. Instead this anchors on
// the fixed argument vocabulary at each end, which is safe because no path in
// the corpus contains those strings.
func parseRecordedCLI(cli string) (inputArgs, outputArgs []string, err error) {
	start := strings.Index(cli, outputArgsStart)
	if start < 0 {
		return nil, nil, fmt.Errorf("no output arguments found in %q", cli)
	}
	end := strings.Index(cli, outputArgsEnd)
	if end < 0 {
		return nil, nil, fmt.Errorf("no %q found in %q", outputArgsEnd, cli)
	}
	outputArgs = strings.Fields(cli[start : end+len(outputArgsEnd)])

	// Input arguments sit between the binary name and " -i ".
	head := cli[:start]
	const binary = "tdarr-ffmpeg"
	if i := strings.Index(head, binary); i >= 0 {
		head = head[i+len(binary):]
	}
	if i := strings.Index(head, " -i "); i >= 0 {
		head = head[:i]
	}
	inputArgs = strings.Fields(head)
	return inputArgs, outputArgs, nil
}
