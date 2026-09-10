// Package decide turns a probe result plus a profile into a plan.
//
// It does no I/O whatsoever: no filesystem, no subprocess, no clock, no
// network. Everything that determines output quality lives here, which is what
// makes it exhaustively testable -- see golden_test.go, which runs every
// function in this package over 1,881 real decisions recorded from the stack
// this project replaces.
//
// If you are ever tempted to stat a file or shell out from this package,
// don't. Pass the answer in instead.
package decide

import (
	"fmt"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/probe"
)

// Action is what will happen to the file as a whole.
type Action string

const (
	// ActionNone means the file is already exactly as the profile wants it.
	ActionNone Action = "none"
	// ActionRemux means streams are copied, dropped or converted, and the
	// video is not re-encoded. Cheap and near-instant.
	ActionRemux Action = "remux"
	// ActionEncode means the video stream is re-encoded. Expensive, and the
	// only action that contends for the iGPU.
	ActionEncode Action = "encode"
	// ActionError means the file could not be planned, e.g. it has no video.
	ActionError Action = "error"
)

// VideoDecision is the fate of the main video stream specifically, kept
// separate from Action because "we declined to encode this" is the single most
// important thing this tool reports. The old stack declined 7,880 files, and
// being able to audit that is the point of the skipped view in the UI.
type VideoDecision string

const (
	// VideoCopy means the video is already the target codec, or is a codec
	// there is nothing to gain by re-encoding.
	VideoCopy VideoDecision = "copy"
	// VideoEncode means the video will be re-encoded.
	VideoEncode VideoDecision = "encode"
	// VideoSkippedFloor means an encode was warranted but the computed target
	// bitrate fell below the profile's floor, so it was declined. This is a
	// quality control, not a failure.
	VideoSkippedFloor VideoDecision = "skipped_floor"
)

// Drop records one stream leaving the output, and why. Purely for reporting;
// the arguments carry the actual instruction.
type Drop struct {
	Kind     string // "audio", "subtitle", "video"
	TypeIdx  int    // index among streams of that kind, as ffmpeg counts them
	Index    int    // absolute stream index in the file
	Codec    string
	Language string
	Title    string
	Reason   string
}

// Change records one stream being converted rather than copied.
type Change struct {
	Kind    string
	TypeIdx int
	From    string
	To      string
	Reason  string
}

// Plan is the complete decision for one file. It is data, not behaviour: the
// encode package takes a Plan and runs it, and --dry-run prints it.
type Plan struct {
	Profile string

	Action Action
	Video  VideoDecision

	// Reason explains a declined encode or an error, and is empty otherwise.
	Reason string

	// Container the output lands in, and the extension that implies. The file
	// keeps its directory and base name; only this may change. See
	// docs/plan.md.
	Container Container

	// SourceContainer is what the file is in now. When it differs from
	// Container the file's extension changes, which is the one rename this
	// tool is allowed to perform.
	SourceContainer string

	// Encoder is the ffmpeg encoder that will be used, and TargetCodec the
	// codec it is expected to produce. TargetCodec is carried on the plan so
	// that the encode package can check the output it actually got without
	// having to be handed the profile as well.
	Encoder     string
	TargetCodec string
	Bitrate     Bitrate

	Drops   []Drop
	Changes []Change

	// Notes record a safety rule overriding the profile. They are meant to be
	// read: a note means the config asked for something this tool declined to
	// do to the file.
	Notes []string

	// InputArgs go before -i, OutputArgs after it. Neither contains the input
	// or output path: the caller supplies those, so that nothing in this
	// package can express an opinion about where a file should live.
	InputArgs  []string
	OutputArgs []string
}

// ExtensionChanges reports whether carrying out this plan renames the file.
func (p Plan) ExtensionChanges() bool {
	return p.Action != ActionNone && p.Action != ActionError &&
		!strings.EqualFold(p.SourceContainer, string(p.Container))
}

// Decide is the whole decision engine.
func Decide(r *probe.Result, p Profile) Plan {
	sourceContainer := r.Container()
	target := resolveContainer(sourceContainer, p)

	plan := Plan{
		Profile:         p.Name,
		Container:       target,
		SourceContainer: sourceContainer,
		TargetCodec:     p.Video.TargetCodec,
		Video:           VideoCopy,
	}

	if _, _, ok := r.MainVideo(); !ok {
		plan.Action = ActionError
		plan.Reason = "no video stream"
		return plan
	}

	// Container limits apply only to a file that is changing container; a file
	// keeping its own has already proved it can hold everything in it. See
	// constraintsFor.
	constraints := constraintsFor(sourceContainer, target)

	// The three sections are built independently and concatenated, which is
	// what the old stack did and therefore what the recorded commands look
	// like.
	video := buildVideo(r, p, target, &plan)
	audio := buildAudio(r, p, constraints, &plan)
	subs := buildSubtitles(r, p, constraints, &plan)

	out := &argList{}
	out.entries = append(out.entries, video.entries...)
	out.entries = append(out.entries, audio.entries...)
	out.entries = append(out.entries, subs.entries...)

	// A large muxing queue costs a little memory and avoids a class of
	// "Too many packets buffered" failures on files with many streams.
	out.preset("-max_muxing_queue_size", "4096")

	if p.Strip.Chapters {
		out.add("-map_chapters", "-1")
	}
	if target == ContainerMP4 {
		// Put the index at the front so the file can start playing before it
		// has been fully fetched.
		out.preset("-movflags", "+faststart")
	}

	plan.OutputArgs = out.flat()
	plan.InputArgs = video.inputArgs

	changed := video.changed || audio.changed || subs.changed
	switch {
	case plan.Video == VideoEncode:
		plan.Action = ActionEncode
	case changed || !strings.EqualFold(sourceContainer, string(target)):
		plan.Action = ActionRemux
	default:
		plan.Action = ActionNone
	}
	return plan
}

// videoSection is an argList plus the input arguments an encoder needs, since
// hardware acceleration is configured before -i.
type videoSection struct {
	argList
	inputArgs []string
}

func buildVideo(r *probe.Result, p Profile, target Container, plan *Plan) videoSection {
	v := videoSection{}

	// -map 0 takes every stream, then the negative maps take things back out.
	// Subtractive mapping means a stream type nobody thought about is kept
	// rather than silently lost.
	v.preset("-map", "0")
	if p.Strip.DataStreams {
		v.preset("-map", "-0:d")
	}
	v.preset("-c:v", "copy")

	for i, s := range r.StreamsOfType(probe.TypeVideo) {
		if isCoverArt(s, p) {
			if p.Strip.CoverArt {
				if p.Quirks.LegacyCoverArtMapSyntax {
					v.add("-map", fmt.Sprintf("-v:%d", i))
				} else {
					v.add("-map", fmt.Sprintf("-0:v:%d", i))
				}
				plan.Drops = append(plan.Drops, Drop{
					Kind: "video", TypeIdx: i, Index: s.Index,
					Codec: s.CodecName, Reason: "cover art",
				})
			}
			// Artwork is never the picture, so it is never a candidate for
			// encoding.
			continue
		}

		if inList(s.CodecName, p.Video.LeaveAlone) {
			continue
		}

		// From here the stream wants encoding. Whether it gets it is the
		// bitrate floor's call.
		br := CalculateBitrate(r, p)
		if br.SourceKbps <= 0 {
			plan.Video = VideoSkippedFloor
			plan.Reason = "could not determine source bitrate"
			continue
		}
		if p.Video.Bitrate.FloorKbps > 0 && br.TargetKbps < p.Video.Bitrate.FloorKbps {
			plan.Video = VideoSkippedFloor
			plan.Bitrate = br
			plan.Reason = fmt.Sprintf("target bitrate %d kbps below floor %d kbps",
				br.TargetKbps, p.Video.Bitrate.FloorKbps)
			continue
		}

		v.remove("-c:v", "copy")
		v.add("-c:v", p.Video.Encoder,
			"-b:v", kbps(br.TargetKbps),
			"-minrate", kbps(br.MinKbps),
			"-maxrate", kbps(br.MaxKbps),
			"-bufsize", kbps(br.BufsizeKbps))

		if isHardwareEncoder(p.Video.Encoder) {
			v.inputArgs = []string{
				"-hwaccel", "vaapi",
				"-hwaccel_device", p.Video.Device,
				"-hwaccel_output_format", "vaapi",
			}
		}

		plan.Video = VideoEncode
		plan.Bitrate = br
		plan.Encoder = p.Video.Encoder
	}

	return v
}

// buildAudio builds the audio section. `constraints` is the container whose
// limits apply, or "" when the file keeps its own container and none do.
func buildAudio(r *probe.Result, p Profile, constraints Container, plan *Plan) argList {
	var a argList
	a.preset("-c:a", "copy")

	streams := r.StreamsOfType(probe.TypeAudio)

	// Decide whether the language tags on this file are worth acting on at
	// all before acting on any of them.
	trustTags := tagsAreCredible(streams, p.Audio.PrimaryLanguages)
	if !trustTags {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"kept every audio track: no track is in %s, so the language tags on "+
				"this file are not trustworthy enough to prune on",
			strings.Join(p.Audio.PrimaryLanguages, "/")))
	}

	// Work out what would be dropped before dropping any of it, so that the
	// last-track guard below can see the whole picture.
	reasons := make([][]string, len(streams))
	for i, s := range streams {
		// Language first. An untagged track is kept when the profile says so,
		// which for every profile here it does -- an untagged track is
		// frequently the one you actually want, especially in the anime
		// library where tags are unreliable.
		if !p.Audio.KeepAllLanguages && trustTags {
			lang := s.Language()
			switch {
			case lang == "" && p.Audio.KeepUntagged:
			case lang == "":
				reasons[i] = append(reasons[i], "untagged and profile drops untagged audio")
			case !inList(lang, p.Audio.KeepLanguages):
				reasons[i] = append(reasons[i], "language "+lang+" not in keep list")
			}
		}

		// Commentary, audio description and SDH tracks are identified by
		// title, which is the only signal available.
		if match, ok := containsFold(s.Title(), p.Audio.DropTitlesMatching); ok {
			reasons[i] = append(reasons[i], "title matches "+match)
		}
	}

	// Never leave a file with no audio.
	//
	// Two files in the corpus have a single audio track tagged chi and aze
	// respectively. A language keep-list applied literally turns those into
	// silent videos, which is the worst thing this tool could do: unwatchable,
	// and unrecoverable once the source is gone. A track in a language nobody
	// asked for is still infinitely better than silence, so the keep-list
	// yields.
	if keeper, ok := lastAudioTrackGuard(streams, reasons); ok {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"kept audio track %d (%s, language %q) even though it matches no keep rule: "+
				"dropping it would leave the file with no audio",
			keeper, streams[keeper].CodecName, streams[keeper].Language()))
		reasons[keeper] = nil
	}

	for i, s := range streams {
		if len(reasons[i]) > 0 {
			for n, reason := range reasons[i] {
				if n > 0 && !p.Quirks.DuplicateStreamDrops {
					break
				}
				a.add("-map", fmt.Sprintf("-0:a:%d", i))
				plan.Drops = append(plan.Drops, Drop{
					Kind: "audio", TypeIdx: i, Index: s.Index, Codec: s.CodecName,
					Language: s.Language(), Title: s.Title(), Reason: reason,
				})
			}
			continue
		}

		// A kept track may still need converting, either because the
		// container cannot hold it or because it is multichannel in a codec
		// the profile normalises.
		if audioPolicy(constraints, s.CodecName) == StreamConvert {
			to := audioConvertTarget(s.Channels, p)
			a.add(fmt.Sprintf("-c:a:%d", i), to)
			plan.Changes = append(plan.Changes, Change{
				Kind: "audio", TypeIdx: i, From: s.CodecName, To: to,
				Reason: string(constraints) + " cannot carry " + s.CodecName,
			})
			continue
		}

		if s.Channels >= p.Audio.MultichannelThreshold &&
			p.Audio.MultichannelTo != "" &&
			!strings.EqualFold(s.CodecName, p.Audio.MultichannelTo) {
			a.add(fmt.Sprintf("-c:a:%d", i), p.Audio.MultichannelTo)
			plan.Changes = append(plan.Changes, Change{
				Kind: "audio", TypeIdx: i, From: s.CodecName, To: p.Audio.MultichannelTo,
				Reason: fmt.Sprintf("%d-channel audio normalised", s.Channels),
			})
		}
	}

	// Tagging comes last so the metadata arguments sit together at the end of
	// the audio section.
	tagUntagged(&a, plan, streams, reasons, "a", p.Audio.TagUntaggedAs)

	return a
}

// tagsAreCredible reports whether a file's language tags look trustworthy
// enough to drop tracks on.
//
// The test is whether any track is in a language this library would actually
// be expected to contain. If none is, the tags are more likely to be wrong
// than the file is to be genuinely unwanted -- see AudioRules.PrimaryLanguages.
func tagsAreCredible(streams []probe.Stream, primary []string) bool {
	if len(primary) == 0 || len(streams) == 0 {
		return true // no expectation configured, so nothing to be suspicious of
	}
	for _, s := range streams {
		lang := s.Language()
		// An untagged track is not evidence either way.
		if lang == "" || lang == "und" {
			continue
		}
		if inList(lang, primary) {
			return true
		}
	}
	return false
}

// tagUntagged writes a language tag onto kept streams that have none, so that
// a player shows something better than "Unknown" in its track picker.
func tagUntagged(a *argList, plan *Plan, streams []probe.Stream, reasons [][]string, specifier, lang string) {
	if lang == "" {
		return
	}
	for i, s := range streams {
		if len(reasons[i]) > 0 {
			continue // being dropped; nothing to tag
		}
		if l := s.Language(); l != "" && l != "und" {
			continue
		}
		a.add(fmt.Sprintf("-metadata:s:%s:%d", specifier, i), "language="+lang)
		plan.Changes = append(plan.Changes, Change{
			Kind: kindFor(specifier), TypeIdx: i, From: s.Language(), To: lang,
			Reason: "untagged track labelled so players can identify it",
		})
	}
}

func kindFor(specifier string) string {
	if specifier == "a" {
		return "audio"
	}
	return "subtitle"
}

// lastAudioTrackGuard reports which track to rescue when the profile's rules
// would drop every audio stream in the file, and whether a rescue is needed.
//
// The rescued track is the one flagged default, falling back to the first,
// which is what a player would have picked anyway.
func lastAudioTrackGuard(streams []probe.Stream, reasons [][]string) (int, bool) {
	if len(streams) == 0 {
		return 0, false
	}
	for i := range streams {
		if len(reasons[i]) == 0 {
			return 0, false // something survives; no rescue needed
		}
	}
	for i := range streams {
		if streams[i].Disposition["default"] == 1 {
			return i, true
		}
	}
	return 0, true
}

// buildSubtitles builds the subtitle section. `constraints` is the container
// whose limits apply, or "" when none do.
func buildSubtitles(r *probe.Result, p Profile, constraints Container, plan *Plan) argList {
	var s argList
	s.preset("-c:s", "copy")

	streams := r.StreamsOfType(probe.TypeSubtitle)
	trustTags := tagsAreCredible(streams, p.Subtitles.PrimaryLanguages)
	kept := make([][]string, len(streams))

	for i, st := range streams {
		dropped := false
		drop := func(reason string) {
			kept[i] = append(kept[i], reason)
			if dropped && !p.Quirks.DuplicateStreamDrops {
				return
			}
			s.add("-map", fmt.Sprintf("-0:s:%d", i))
			plan.Drops = append(plan.Drops, Drop{
				Kind: "subtitle", TypeIdx: i, Index: st.Index, Codec: st.CodecName,
				Language: st.Language(), Title: st.Title(), Reason: reason,
			})
			dropped = true
		}

		if inList(st.CodecName, p.Subtitles.DropCodecs) {
			drop("codec " + st.CodecName + " not usable")
			continue
		}

		// A forced track is the translated-signage track for a film watched in
		// its original audio. Losing one is very visible, so it outranks the
		// language list.
		forcedKeep := p.Subtitles.KeepForced && st.Forced()

		if !forcedKeep {
			lang := st.Language()
			switch {
			case !trustTags:
				// Tags on this file are not credible; see tagsAreCredible.
			case lang == "" && p.Subtitles.KeepUntagged:
			case lang == "":
				drop("untagged and profile drops untagged subtitles")
			case !inList(lang, p.Subtitles.KeepLanguages):
				drop("language " + lang + " not in keep list")
			}

			if match, ok := containsFold(st.Title(), p.Subtitles.DropTitlesMatching); ok {
				drop("title matches " + match)
			}
		}

		if dropped {
			continue
		}

		action, to := subtitlePolicy(constraints, st.CodecName)
		switch action {
		case StreamDrop:
			drop(string(constraints) + " cannot carry " + st.CodecName + " subtitles")
		case StreamConvert:
			s.add(fmt.Sprintf("-c:s:%d", i), to)
			plan.Changes = append(plan.Changes, Change{
				Kind: "subtitle", TypeIdx: i, From: st.CodecName, To: to,
				Reason: string(constraints) + " requires " + to,
			})
		}
	}

	tagUntagged(&s, plan, streams, kept, "s", p.Subtitles.TagUntaggedAs)

	return s
}

// isCoverArt reports whether a video stream is embedded artwork rather than
// the picture.
//
// The compat quirk narrows this to MJPEG alone, which is all the old stack
// recognised. Everything else it saw as a video stream to be encoded -- see
// Quirks.CoverArtMJPEGOnly.
func isCoverArt(s probe.Stream, p Profile) bool {
	if p.Quirks.CoverArtMJPEGOnly {
		return strings.EqualFold(s.CodecName, "mjpeg")
	}
	return s.IsCoverArt()
}

func kbps(v int) string { return fmt.Sprintf("%dk", v) }

func isHardwareEncoder(encoder string) bool {
	return strings.HasSuffix(encoder, "_vaapi") || strings.HasSuffix(encoder, "_qsv")
}
