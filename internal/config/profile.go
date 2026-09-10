package config

import (
	"bytes"
	"fmt"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"gopkg.in/yaml.v3"
)

// This file is the YAML shape of a profile and how `extends` is resolved.
//
// The shape mirrors decide.Profile almost field for field, on purpose: the
// point of the config file is that you can read it and know what will happen,
// and that only holds while the knobs in the file are the knobs the decision
// engine actually has.
//
// Two things in decide.Profile are deliberately *not* here:
//
//   - Quirks, which are bug-for-bug compatibility switches for the stack this
//     replaces. They exist so the golden corpus can prove this project
//     reproduces the old arithmetic, and enabling one in production would mean
//     asking for known bugs. Every profile gets the corrected values.
//   - KeepAllLanguages, which is part of that same compatibility shim.

type profileYAML struct {
	Extends   string       `yaml:"extends"`
	Container string       `yaml:"container"`
	Video     videoYAML    `yaml:"video"`
	Audio     audioYAML    `yaml:"audio"`
	Subtitles subtitleYAML `yaml:"subtitles"`
	Strip     stripYAML    `yaml:"strip"`
}

type videoYAML struct {
	TargetCodec string   `yaml:"target_codec"`
	Encoder     string   `yaml:"encoder"`
	Device      string   `yaml:"device"`
	LeaveAlone  []string `yaml:"leave_alone"`

	SourceBitrateBasis string      `yaml:"source_bitrate_basis"`
	Bitrate            bitrateYAML `yaml:"bitrate"`
}

type bitrateYAML struct {
	Tiers         []tierYAML `yaml:"tiers"`
	MinMultiplier float64    `yaml:"min_multiplier"`
	MaxMultiplier float64    `yaml:"max_multiplier"`

	// FloorKbps is a pointer so that "I did not set this" and "I set it to
	// zero" are different answers. They matter more here than anywhere else
	// in the file: the floor is the reason the old stack never produced a
	// bad-looking file, and a forgotten key silently defaulting to no floor
	// would send several thousand files into an encode they should have been
	// declined for.
	FloorKbps *int `yaml:"floor_kbps"`
}

type tierYAML struct {
	Above   int     `yaml:"above"`
	Divisor float64 `yaml:"divisor"`
}

type audioYAML struct {
	MultichannelTo        string   `yaml:"multichannel_to"`
	MultichannelThreshold int      `yaml:"multichannel_threshold"`
	KeepLanguages         []string `yaml:"keep_languages"`
	KeepUntagged          *bool    `yaml:"keep_untagged"`
	DropTitlesMatching    []string `yaml:"drop_titles_matching"`
	PrimaryLanguages      []string `yaml:"primary_languages"`
	TagUntaggedAs         string   `yaml:"tag_untagged_as"`
}

type subtitleYAML struct {
	KeepLanguages      []string `yaml:"keep_languages"`
	KeepUntagged       *bool    `yaml:"keep_untagged"`
	DropTitlesMatching []string `yaml:"drop_titles_matching"`
	DropCodecs         []string `yaml:"drop_codecs"`
	PrimaryLanguages   []string `yaml:"primary_languages"`
	TagUntaggedAs      string   `yaml:"tag_untagged_as"`
	KeepForced         *bool    `yaml:"keep_forced"`
}

type stripYAML struct {
	DataStreams bool `yaml:"data_streams"`
	CoverArt    bool `yaml:"cover_art"`
	Chapters    bool `yaml:"chapters"`
}

// resolver walks the profile nodes, following `extends`, and memoises what it
// has already built.
type resolver struct {
	nodes map[string]yaml.Node
	done  map[string]profileYAML
	stack []string // the chain currently being resolved, for cycle detection
	p     *problems
}

// resolveProfiles turns the raw profile nodes into finished decide.Profiles.
func resolveProfiles(nodes map[string]yaml.Node, defaultContainer string, p *problems) map[string]decide.Profile {
	r := &resolver{nodes: nodes, done: map[string]profileYAML{}, p: p}

	out := make(map[string]decide.Profile, len(nodes))
	for name := range nodes {
		raw, ok := r.resolve(name)
		if !ok {
			continue
		}
		prof := toDecide(name, raw)
		if prof.Container == "" {
			prof.Container = decide.Container(defaultContainer)
		}
		validateProfile(name, raw, prof, p)
		out[name] = prof
	}
	return out
}

// resolve builds one profile, parent first.
//
// The merge is done by decoding the parent's YAML into a struct and then
// decoding the child's YAML into that same struct. yaml.v3 only writes the
// fields a document actually mentions, so the child overrides exactly what it
// names and inherits the rest. A list in the child replaces the parent's list
// rather than appending to it, which is what you want for a keep-list: adding
// `keep_languages: [eng]` should mean "only English", not "English as well".
func (r *resolver) resolve(name string) (profileYAML, bool) {
	if done, ok := r.done[name]; ok {
		return done, true
	}
	node, ok := r.nodes[name]
	if !ok {
		r.p.addf("profile %q extends %q, which is not defined", r.parent(), name)
		return profileYAML{}, false
	}
	for _, seen := range r.stack {
		if seen == name {
			r.p.addf("profiles: extends cycle: %s -> %s", joinArrow(r.stack), name)
			return profileYAML{}, false
		}
	}

	// Read `extends` on its own first: the parent has to be built before the
	// child can be decoded on top of it.
	var head struct {
		Extends string `yaml:"extends"`
	}
	if err := node.Decode(&head); err != nil {
		r.p.addf("profiles.%s (line %d): %s", name, node.Line, yamlProblem(err))
		return profileYAML{}, false
	}

	var merged profileYAML
	if head.Extends != "" {
		r.stack = append(r.stack, name)
		parent, ok := r.resolve(head.Extends)
		r.stack = r.stack[:len(r.stack)-1]
		if !ok {
			return profileYAML{}, false
		}
		merged = parent
	}

	if err := decodeNodeStrict(node, &merged); err != nil {
		r.p.addf("profiles.%s (line %d): %s", name, node.Line, yamlProblem(err))
		return profileYAML{}, false
	}
	merged.Extends = head.Extends

	r.done[name] = merged
	return merged, true
}

func (r *resolver) parent() string {
	if len(r.stack) == 0 {
		return "?"
	}
	return r.stack[len(r.stack)-1]
}

// decodeNodeStrict decodes a node with unknown-field checking on.
//
// yaml.Node.Decode has no strict mode, so the node is written back out and
// read in through a Decoder that does. Config files are a few kilobytes, so
// the round trip costs nothing and it means a typo inside a profile is caught
// exactly like a typo anywhere else in the document.
func decodeNodeStrict(node yaml.Node, into any) error {
	data, err := yaml.Marshal(&node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(into)
}

// toDecide converts the YAML shape into the engine's profile.
func toDecide(name string, y profileYAML) decide.Profile {
	basis := decide.BitrateBasis(y.Video.SourceBitrateBasis)
	if basis == "" {
		// The container basis is what the old stack used and what produced
		// the output being reproduced; see docs/tdarr-analysis.md.
		basis = decide.BasisContainer
	}

	tiers := make([]decide.Tier, 0, len(y.Video.Bitrate.Tiers))
	for _, t := range y.Video.Bitrate.Tiers {
		tiers = append(tiers, decide.Tier{AboveKbps: t.Above, Divisor: t.Divisor})
	}

	return decide.Profile{
		Name:      name,
		Container: decide.Container(y.Container),
		Video: decide.VideoRules{
			TargetCodec: y.Video.TargetCodec,
			Encoder:     y.Video.Encoder,
			Device:      y.Video.Device,
			LeaveAlone:  y.Video.LeaveAlone,
			Bitrate: decide.BitrateRules{
				Basis:         basis,
				Tiers:         tiers,
				MinMultiplier: y.Video.Bitrate.MinMultiplier,
				MaxMultiplier: y.Video.Bitrate.MaxMultiplier,
				FloorKbps:     intOr(y.Video.Bitrate.FloorKbps, 0),
			},
		},
		Audio: decide.AudioRules{
			MultichannelTo:        y.Audio.MultichannelTo,
			MultichannelThreshold: y.Audio.MultichannelThreshold,
			KeepLanguages:         y.Audio.KeepLanguages,
			// When in doubt, keep it: an untagged track is kept unless the
			// file explicitly says otherwise.
			KeepUntagged:       boolOr(y.Audio.KeepUntagged, true),
			DropTitlesMatching: y.Audio.DropTitlesMatching,
			PrimaryLanguages:   y.Audio.PrimaryLanguages,
			TagUntaggedAs:      y.Audio.TagUntaggedAs,
		},
		Subtitles: decide.SubtitleRules{
			KeepLanguages:      y.Subtitles.KeepLanguages,
			KeepUntagged:       boolOr(y.Subtitles.KeepUntagged, true),
			DropTitlesMatching: y.Subtitles.DropTitlesMatching,
			DropCodecs:         y.Subtitles.DropCodecs,
			PrimaryLanguages:   y.Subtitles.PrimaryLanguages,
			TagUntaggedAs:      y.Subtitles.TagUntaggedAs,
			KeepForced:         boolOr(y.Subtitles.KeepForced, true),
		},
		Strip: decide.StripRules{
			DataStreams: y.Strip.DataStreams,
			CoverArt:    y.Strip.CoverArt,
			Chapters:    y.Strip.Chapters,
		},
		// Not configurable, on purpose. See the note at the top of this file.
		Quirks: decide.Quirks{MinuteFactor: 1.0 / 60.0},
	}
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func intOr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func joinArrow(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " -> "
		}
		out += v
	}
	return fmt.Sprint(out)
}
