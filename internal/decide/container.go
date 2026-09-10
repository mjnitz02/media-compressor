package decide

import "strings"

// This file answers one question: when a file is being moved into a different
// container, what can each stream do there?
//
// The "being moved" part is essential. A stream sitting in a container is
// proof that the container can hold it, so a file that keeps its own container
// never has anything forced on it -- see constraintsFor. Container rules only
// ever apply to a file that is changing container, which is the only situation
// in which a stream can turn out to have nowhere to go.
//
// MKV can carry everything that turns up in practice, so moving into MKV
// costs nothing. Moving into MP4 does: image-based subtitles have no MP4
// representation at all and must be dropped, and text subtitles have to be
// rewritten as mov_text.

// StreamAction is what happens to one stream.
type StreamAction string

const (
	// StreamCopy passes the stream through untouched.
	StreamCopy StreamAction = "copy"
	// StreamConvert re-encodes the stream to a different codec.
	StreamConvert StreamAction = "convert"
	// StreamDrop leaves the stream out of the output.
	StreamDrop StreamAction = "drop"
)

// SupportedContainers are the containers this tool will write.
var SupportedContainers = []string{"mkv", "mp4"}

// Extension is the file extension for a container, including the dot.
func (c Container) Extension() string { return "." + string(c) }

// resolveContainer turns the profile's container setting into a concrete
// output container for this file.
//
// ContainerSource means "do not remux just to change containers": a file that
// is already MKV stays MKV and one that is already MP4 stays MP4, whether or
// not it is also being re-encoded. This is the setting for a library holding a
// mix of both, where forcing everything to one container would rename half of
// it for no benefit.
//
// It is only honoured for containers this tool writes. Anything exotic (avi,
// wmv, vob) still has to land somewhere, and that is MKV -- the one case where
// ContainerSource changes an extension.
func resolveContainer(sourceContainer string, p Profile) Container {
	if p.Container != ContainerSource {
		return p.Container
	}
	if inList(sourceContainer, SupportedContainers) {
		return Container(strings.ToLower(sourceContainer))
	}
	return ContainerMKV
}

// mp4SafeAudio are audio codecs MP4 carries with broad client support.
var mp4SafeAudio = []string{"aac", "ac3", "eac3", "mp3", "alac", "opus", "dts"}

// mp4TextSubtitles are text subtitle codecs that can become mov_text.
//
// ASS and SSA are in here reluctantly: converting them to mov_text keeps the
// dialogue but throws away positioning, fonts and karaoke styling. For an
// anime library that is a real loss, which is a good reason to leave those
// libraries on MKV.
var mp4TextSubtitles = []string{"subrip", "srt", "ass", "ssa", "mov_text", "text", "webvtt", "subviewer", "microdvd"}

// constraintsFor returns the container whose limits apply to this file's
// streams, or "" when none do.
//
// None do whenever the file keeps the container it already has. Everything in
// the file demonstrably muxes into it, so there is nothing to force -- and
// forcing anything would mean a lossy re-encode of a track that was fine. An
// MP4 holding FLAC is the case that matters: FLAC in MP4 is legal and plays,
// so converting it to AAC on the way back into MP4 would throw away quality
// for no reason at all.
func constraintsFor(sourceContainer string, target Container) Container {
	if strings.EqualFold(sourceContainer, string(target)) {
		return ""
	}
	return target
}

// audioPolicy decides what the container forces on an audio stream, before the
// profile's own multichannel rule is applied.
//
// A zero Container means no container change is happening and so nothing is
// forced.
func audioPolicy(c Container, codec string) StreamAction {
	if c != ContainerMP4 {
		return StreamCopy
	}
	if inList(codec, mp4SafeAudio) {
		return StreamCopy
	}
	// TrueHD, FLAC, Vorbis and raw PCM either cannot be muxed into MP4 or are
	// not playable once they are. Converting is the only way to keep the
	// track, and keeping the track is the priority.
	return StreamConvert
}

// subtitlePolicy decides what the container forces on a subtitle stream.
func subtitlePolicy(c Container, codec string) (StreamAction, string) {
	codec = strings.ToLower(codec)
	switch c {
	case "":
		// The container is not changing, so nothing is forced.
		return StreamCopy, ""
	case ContainerMKV:
		// mov_text is an MP4-native format that Matroska does not really
		// want; rewrite it as SubRip on the way in.
		if codec == "mov_text" {
			return StreamConvert, "subrip"
		}
		return StreamCopy, ""
	case ContainerMP4:
	default:
		return StreamCopy, ""
	}
	if inList(codec, mp4TextSubtitles) {
		if codec == "mov_text" {
			return StreamCopy, ""
		}
		return StreamConvert, "mov_text"
	}
	// PGS, VOBSUB, DVB and teletext are bitmap formats with no MP4 mapping.
	// There is nothing to convert them to short of burning them into the
	// video, which this tool will not do.
	return StreamDrop, ""
}

// audioConvertTarget is the codec an audio stream should be converted to,
// given how many channels it has.
func audioConvertTarget(channels int, p Profile) string {
	if channels >= p.Audio.MultichannelThreshold {
		return p.Audio.MultichannelTo
	}
	// Stereo and mono go to AAC: it is the most broadly played stereo codec in
	// an MP4 and costs less than AC-3 at the same quality.
	return "aac"
}
