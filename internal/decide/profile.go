package decide

// A Profile is the work: what to encode to, which tracks to keep, and which
// container to land in. A Library (elsewhere) is only a name, a profile
// reference, and some paths. Keeping those two apart is the whole reason this
// project exists -- see docs/libraries.md.
type Profile struct {
	Name string

	// Container the output should end up in. The file never leaves its
	// directory and never changes its base name; only the extension may
	// change. See docs/plan.md.
	Container Container

	Video     VideoRules
	Audio     AudioRules
	Subtitles SubtitleRules
	Strip     StripRules
	Quirks    Quirks
}

// Container is an output container choice.
type Container string

const (
	// ContainerMKV always lands in Matroska. It holds every codec that turns
	// up in practice, so nothing ever has to be dropped for the container's
	// sake.
	ContainerMKV Container = "mkv"

	// ContainerMP4 always lands in MP4. Cheaper for direct play on some
	// clients, but it cannot carry image-based subtitles (PGS, VOBSUB) or
	// TrueHD, so choosing it means accepting that those tracks are dropped or
	// converted. Suitable for libraries that are video+audio with burnt-in
	// subtitles.
	ContainerMP4 Container = "mp4"

	// ContainerSource keeps whatever container the file already has, when it
	// is one this tool supports, and so never remuxes purely to change
	// containers. The conservative choice for a mixed library.
	ContainerSource Container = "source"
)

// VideoRules covers the encode decision and the bitrate math.
type VideoRules struct {
	TargetCodec string // "hevc"
	Encoder     string // hevc_vaapi | hevc_qsv | libx265
	Device      string // /dev/dri/renderD128, for the VAAPI/QSV encoders

	// LeaveAlone are video codecs that are never re-encoded, because doing so
	// would only lose quality. HEVC is here because it is the target. AV1 is
	// here because it is *more* efficient than HEVC, so transcoding it down is
	// a straight downgrade -- something the old stack would happily have done.
	LeaveAlone []string

	Bitrate BitrateRules
}

// BitrateRules is the tier table and the skip floor.
type BitrateRules struct {
	Basis BitrateBasis
	Tiers []Tier

	MinMultiplier float64 // -minrate, as a fraction of target
	MaxMultiplier float64 // -maxrate

	// FloorKbps declines the encode when the computed target lands below it.
	// This is the single most important setting in the file. The old stack
	// skipped 7,880 files on this rule, which is the main reason it never
	// produced a bad-looking output: it simply refused the hard jobs.
	FloorKbps int
}

// BitrateBasis selects what "the source bitrate" means.
type BitrateBasis string

const (
	// BasisContainer divides total file size by duration, charging audio and
	// subtitle bytes to the video budget. Over-generous, and the default,
	// because it is what produced the output quality being replicated.
	BasisContainer BitrateBasis = "container"

	// BasisVideoStream uses the video stream's own bitrate when the file
	// reports one. Honest, and about 10% tighter in practice.
	BasisVideoStream BitrateBasis = "video_stream"
)

// Tier maps a source bitrate to how aggressively it is cut. First tier whose
// AboveKbps the source reaches wins, scanning from the top of the slice.
type Tier struct {
	AboveKbps int
	Divisor   float64
}

// AudioRules covers track selection and multichannel handling.
type AudioRules struct {
	// MultichannelTo is the codec multichannel audio is converted to, and
	// MultichannelThreshold is the channel count at which a track counts as
	// multichannel. 6 means 5.1 and up.
	MultichannelTo        string
	MultichannelThreshold int

	KeepLanguages      []string
	KeepUntagged       bool
	DropTitlesMatching []string

	// PrimaryLanguages are the languages you would expect this library to
	// actually contain. If a file has no audio track in any of them, the
	// language tags are treated as untrustworthy and no track is dropped for
	// its language.
	//
	// This is the "when in doubt, keep it" rule made concrete. An anime
	// release turning up with a Korean track and no Japanese one is far more
	// likely to be mistagged than to genuinely have nothing you want, and
	// pruning it on the strength of those tags is how you end up with a file
	// nobody can watch. When the expected languages *are* present, the tags
	// look credible and the extras get pruned as normal.
	PrimaryLanguages []string

	// TagUntaggedAs writes a language tag onto tracks that have none, so that
	// Plex and other players show something useful in the track picker
	// instead of "Unknown".
	//
	// This is worth understanding before enabling: it makes otherwise
	// untouched files need a remux, so switching it on means one cheap,
	// lossless pass over the library. It never re-encodes anything.
	TagUntaggedAs string

	// KeepAllLanguages disables language-based dropping entirely. The old
	// stack's encoder plugin did no audio language filtering -- that was a
	// separate plugin in the chain -- so the compat profile sets this.
	KeepAllLanguages bool
}

// SubtitleRules covers subtitle track selection.
type SubtitleRules struct {
	KeepLanguages      []string
	KeepUntagged       bool
	DropTitlesMatching []string
	DropCodecs         []string

	// PrimaryLanguages and TagUntaggedAs work exactly as they do for audio.
	PrimaryLanguages []string
	TagUntaggedAs    string

	// KeepForced keeps a forced track whatever its language. Forced subtitles
	// are usually the translated-signage track for a film you are watching in
	// its original audio, so losing one is very visible.
	KeepForced bool
}

// StripRules covers the always-on cleanup.
type StripRules struct {
	DataStreams bool
	CoverArt    bool
	Chapters    bool
}

// Quirks are bug-for-bug compatibility switches for the stack being replaced.
//
// They exist for one reason: the golden corpus in testdata/ records 1,881 real
// decisions from that stack, and the only way to prove this project reproduces
// its quality is to reproduce its arithmetic exactly, quirks included. Turning
// a quirk off is a deliberate improvement, and it shows up as a reviewed diff
// in the golden expectations rather than as a silent behaviour change.
type Quirks struct {
	// MinuteFactor converts seconds to minutes. The correct value is 1.0/60.
	// Tdarr used the constant 0.0166667, which is very slightly larger, and
	// so computed very slightly lower bitrates. It moves a bitrate by well
	// under a tenth of a percent, but at a truncation boundary that is enough
	// to change the integer, which is why matching the corpus needs it.
	MinuteFactor float64

	// CoverArtMJPEGOnly reproduces the old stack recognising only MJPEG as
	// embedded artwork. Any other still-image stream was neither stripped nor
	// recognised, so it fell through into the video encode branch: twelve
	// corpus fixtures are HEVC files that were selected for a video encode on
	// the strength of an attached PNG poster, and were saved from it only by
	// the bitrate floor.
	CoverArtMJPEGOnly bool

	// LegacyCoverArtMapSyntax emits `-map -v:N` instead of the correct
	// `-map -0:v:N`. ffmpeg accepts both, so this is cosmetic, but the corpus
	// records the former.
	LegacyCoverArtMapSyntax bool

	// DuplicateStreamDrops emits a second `-map -0:s:N` when one stream is
	// dropped for two reasons at once, e.g. an unwanted language that is also
	// titled "Commentary". ffmpeg ignores the repeat, so this too is
	// cosmetic, but 44 of the 1,877 recorded commands contain it.
	DuplicateStreamDrops bool
}

// Standard is the default profile: the old stack's bitrate math and skip
// floor, with its genuine mistakes corrected.
//
// Track selection is deliberately generous. Dropping audio you wanted is
// unrecoverable and keeping a track you did not costs a few megabytes, so
// every ambiguous case here resolves to "keep".
func Standard() Profile {
	return Profile{
		Name:      "standard",
		Container: ContainerMKV,
		Video: VideoRules{
			TargetCodec: "hevc",
			Encoder:     "hevc_vaapi",
			Device:      "/dev/dri/renderD128",
			LeaveAlone:  []string{"hevc", "av1"},
			Bitrate: BitrateRules{
				Basis: BasisContainer,
				Tiers: []Tier{
					{AboveKbps: 10000, Divisor: 2.0},
					{AboveKbps: 6000, Divisor: 1.75},
					{AboveKbps: 3000, Divisor: 1.5},
					{AboveKbps: 0, Divisor: 1.0},
				},
				MinMultiplier: 0.7,
				MaxMultiplier: 1.3,
				FloorKbps:     3000,
			},
		},
		Audio: AudioRules{
			MultichannelTo:        "ac3",
			MultichannelThreshold: 6,
			KeepLanguages:         []string{"eng", "und", "jpn", "kor", "fra", "fre"},
			KeepUntagged:          true,
			DropTitlesMatching:    []string{"commentary", "description", "sdh"},
			PrimaryLanguages:      []string{"eng"},
			TagUntaggedAs:         "eng",
		},
		Subtitles: SubtitleRules{
			KeepLanguages:      []string{"eng", "und", "jpn", "kor"},
			KeepUntagged:       true,
			DropTitlesMatching: []string{"commentary", "description", "sdh"},
			DropCodecs:         []string{"eia_608"},
			KeepForced:         true,
			PrimaryLanguages:   []string{"eng"},
			TagUntaggedAs:      "eng",
		},
		Strip: StripRules{
			DataStreams: true,
			CoverArt:    true,
			Chapters:    false,
		},
		Quirks: Quirks{
			MinuteFactor: 1.0 / 60.0,
		},
	}
}

// Anime is Standard biased harder toward keeping tracks.
//
// Anime language tags in this library are unreliable -- English is sometimes
// tagged jpn and the reverse -- so a language tag is weak evidence and
// dropping on it is risky. Soft subtitles are the norm and usually wanted, so
// only commentary goes. See docs/libraries.md.
func Anime() Profile {
	p := Standard()
	p.Name = "anime"
	p.Audio.KeepLanguages = []string{"eng", "und", "jpn", "kor"}
	p.Subtitles.KeepLanguages = []string{"eng", "und", "jpn", "kor"}

	// Either an English or a Japanese track is enough to believe the tags on
	// an anime release. A file with neither -- a lone Korean track, say -- is
	// left entirely alone rather than pruned on tags that are probably wrong.
	p.Audio.PrimaryLanguages = []string{"eng", "jpn"}
	p.Subtitles.PrimaryLanguages = []string{"eng", "jpn"}

	// Untagged anime audio is far more often Japanese than not.
	p.Audio.TagUntaggedAs = "jpn"
	p.Subtitles.TagUntaggedAs = "eng"

	// SDH subtitles are frequently the only English track on an anime release,
	// so unlike Standard they are kept.
	p.Subtitles.DropTitlesMatching = []string{"commentary", "description"}
	return p
}

// TdarrCompat reproduces the old stack's encoder plugin
// (Tdarr_Plugin_drdd_standardise_all_in_one) bug-for-bug.
//
// It exists so the golden corpus test can assert that this project makes the
// same decision as the old stack on all 1,881 recorded files. It is not meant
// to be configured in production -- Standard is. The differences are the whole
// point, and each one is a named quirk or a commented field below.
func TdarrCompat() Profile {
	p := Standard()
	p.Name = "tdarr-compat"

	// The plugin only ever wrote MKV.
	p.Container = ContainerMKV

	// It re-encoded anything that was not already HEVC, AV1 included. All 28
	// AV1 fixtures in the corpus happened to fall below the bitrate floor, so
	// this downgrade never actually fired -- but only by luck.
	p.Video.LeaveAlone = []string{"hevc"}

	// Audio language filtering was a different plugin in the chain
	// (MC93_Migz3CleanAudio), so it is not attributable to these fixtures.
	// Commentary-title dropping was in this plugin and stays on.
	p.Audio.KeepAllLanguages = true

	// Neither the tag-trust gate nor language tagging existed in this plugin.
	p.Audio.PrimaryLanguages = nil
	p.Audio.TagUntaggedAs = ""
	p.Subtitles.PrimaryLanguages = nil
	p.Subtitles.TagUntaggedAs = ""

	// The plugin ignored the forced disposition.
	p.Subtitles.KeepForced = false

	p.Quirks = Quirks{
		MinuteFactor:            0.0166667,
		CoverArtMJPEGOnly:       true,
		LegacyCoverArtMapSyntax: true,
		DuplicateStreamDrops:    true,
	}
	return p
}
