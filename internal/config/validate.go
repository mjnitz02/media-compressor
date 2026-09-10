package config

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// Validation is deliberately noisy about things that would otherwise fail
// quietly and expensively. The guiding question for each check is: if this
// were wrong, when would you find out?
//
// A misspelled profile name fails at startup, which is fine. A misspelled
// language code does not fail at all -- it just silently stops matching, and
// you find out when someone reports that a film has no English audio, months
// later, with the source file long deleted. Those are the checks worth having.

// validate covers everything outside the profiles themselves.
func validate(c *Config, f file, p *problems) {
	if len(f.Profiles) == 0 {
		p.addf("profiles: none defined")
	}
	if len(f.Libraries) == 0 {
		p.addf("libraries: none defined; there is nothing to do")
	}
	if !validContainer(c.Defaults.Container) {
		p.addf("defaults.container: %q is not one of mkv, mp4, source", c.Defaults.Container)
	}
	if _, ok := c.Profiles[c.Defaults.Profile]; !ok && len(c.Profiles) > 0 {
		p.addf("defaults.profile: %q is not defined (have: %s)", c.Defaults.Profile, nameList(c.Profiles))
	}
	if c.Defaults.Workers.Remux < 1 {
		p.addf("defaults.workers.remux: must be at least 1")
	}
	if c.Defaults.Workers.Encode < 1 {
		p.addf("defaults.workers.encode: must be at least 1")
	}

	validateLibraryPaths(c, p)
	validateScanner(c, p)

	if c.Paths.WorkDir == "" {
		p.addf("paths.work_dir: required")
	}
	if c.Paths.Database == "" {
		p.addf("paths.database: required")
	}
	if c.Server.Listen == "" {
		p.addf("server.listen: required")
	}
}

// validateLibraryPaths checks names and, more importantly, that no two
// libraries claim the same tree.
func validateLibraryPaths(c *Config, p *problems) {
	seenName := map[string]bool{}
	type owned struct{ lib, path string }
	var all []owned

	for _, lib := range c.Libraries {
		switch {
		case lib.Name == "":
			p.addf("libraries: a library has no name")
		case seenName[lib.Name]:
			p.addf("library %s: defined twice", lib.Name)
		}
		seenName[lib.Name] = true

		if len(lib.Paths) == 0 {
			p.addf("library %s: no paths", lib.Name)
		}
		for _, path := range lib.Paths {
			if !filepath.IsAbs(path) {
				p.addf("library %s: path %q must be absolute", lib.Name, path)
				continue
			}
			all = append(all, owned{lib.Name, filepath.Clean(path)})
		}
	}

	// Two libraries covering the same tree means one file has two profiles,
	// and which one wins would come down to scan order. That is exactly the
	// kind of thing that must not be discovered from its output.
	for i, a := range all {
		for j, b := range all {
			if i >= j {
				continue
			}
			if !pathContains(a.path, b.path) && !pathContains(b.path, a.path) {
				continue
			}
			if a.lib == b.lib {
				p.addf("library %s: paths %s and %s overlap; the inner one would be scanned twice", a.lib, a.path, b.path)
			} else {
				p.addf("libraries %s and %s overlap on %s and %s; a file there would have two profiles", a.lib, b.lib, a.path, b.path)
			}
		}
	}
}

// pathContains reports whether parent is a, or an ancestor of, child.
func pathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	if !strings.HasSuffix(parent, string(filepath.Separator)) {
		parent += string(filepath.Separator)
	}
	return strings.HasPrefix(child, parent)
}

func validateScanner(c *Config, p *problems) {
	if len(c.Scanner.Extensions) == 0 {
		p.addf("scanner.extensions: none listed; no file would ever be considered")
	}
	for _, ext := range c.Scanner.Extensions {
		if strings.HasPrefix(ext, ".") {
			p.addf("scanner.extensions: %q should be written without the leading dot", ext)
		}
		if strings.ToLower(ext) != ext {
			p.addf("scanner.extensions: %q should be lowercase; matching is case-insensitive", ext)
		}
	}
	for _, g := range c.Scanner.IgnoreGlobs {
		// filepath.Match only reports a syntax error, which is all that is
		// being checked here: it does not understand `**`, so the matching
		// itself belongs to the scanner in Phase 4.
		if _, err := filepath.Match(g, "probe"); err != nil {
			p.addf("scanner.ignore_globs: %q is not a valid pattern: %v", g, err)
		}
	}
	if c.Scanner.MinAgeSeconds < 0 {
		p.addf("scanner.min_age_seconds: must not be negative")
	}
	if c.Scanner.IntervalMinutes < 1 {
		p.addf("scanner.interval_minutes: must be at least 1")
	}
}

// languageTag is the shape of an ISO-639 code as ffprobe reports it: three
// letters usually (eng, jpn, und), two sometimes, occasionally with a region
// suffix like pt-br.
var languageTag = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,4})?$`)

// validateProfile checks one resolved profile.
func validateProfile(name string, raw profileYAML, prof decide.Profile, p *problems) {
	at := func(field string) string { return "profiles." + name + "." + field }

	if raw.Container != "" && !validContainer(raw.Container) {
		p.addf("%s: %q is not one of mkv, mp4, source", at("container"), raw.Container)
	}

	v := prof.Video
	if v.TargetCodec == "" {
		p.addf("%s: required", at("video.target_codec"))
	}
	if v.Encoder == "" {
		p.addf("%s: required", at("video.encoder"))
	}
	if isHardwareEncoder(v.Encoder) && v.Device == "" {
		p.addf("%s: required for the hardware encoder %s", at("video.device"), v.Encoder)
	}
	switch {
	case len(v.LeaveAlone) == 0:
		p.addf("%s: required; it must at least contain the target codec, or every file would be re-encoded forever", at("video.leave_alone"))
	case v.TargetCodec != "" && !containsFold(v.LeaveAlone, v.TargetCodec):
		p.addf("%s: must contain the target codec %q, or an already-converted file would be re-encoded on every scan", at("video.leave_alone"), v.TargetCodec)
	}

	switch prof.Video.Bitrate.Basis {
	case decide.BasisContainer, decide.BasisVideoStream:
	default:
		p.addf("%s: %q is not one of container, video_stream", at("video.source_bitrate_basis"), raw.Video.SourceBitrateBasis)
	}

	validateTiers(at, prof.Video.Bitrate, raw.Video.Bitrate, p)
	validateAudio(at, prof.Audio, p)
	validateSubtitles(at, prof.Subtitles, p)
}

func validateTiers(at func(string) string, b decide.BitrateRules, raw bitrateYAML, p *problems) {
	if len(b.Tiers) == 0 {
		p.addf("%s: required", at("video.bitrate.tiers"))
	}
	for i, t := range b.Tiers {
		if t.Divisor <= 0 {
			p.addf("%s: tier %d (above %d) has divisor %v; must be greater than 0", at("video.bitrate.tiers"), i, t.AboveKbps, t.Divisor)
		}
		if t.Divisor > 0 && t.Divisor < 1 {
			p.addf("%s: tier %d (above %d) has divisor %v, which would raise the bitrate above the source", at("video.bitrate.tiers"), i, t.AboveKbps, t.Divisor)
		}
		// The engine takes the first tier the source bitrate reaches,
		// scanning the list in order, so an out-of-order table silently
		// applies the wrong divisor to everything below the mistake.
		if i > 0 && t.AboveKbps >= b.Tiers[i-1].AboveKbps {
			p.addf("%s: must be ordered highest `above` first; tier %d (above %d) is not below tier %d (above %d)",
				at("video.bitrate.tiers"), i, t.AboveKbps, i-1, b.Tiers[i-1].AboveKbps)
		}
		if t.AboveKbps < 0 {
			p.addf("%s: tier %d has a negative `above`", at("video.bitrate.tiers"), i)
		}
	}
	if n := len(b.Tiers); n > 0 && b.Tiers[n-1].AboveKbps != 0 {
		p.addf("%s: the last tier must be `above: 0` so that every bitrate matches one", at("video.bitrate.tiers"))
	}

	if b.MinMultiplier <= 0 {
		p.addf("%s: required, and must be greater than 0", at("video.bitrate.min_multiplier"))
	}
	if b.MaxMultiplier <= 0 {
		p.addf("%s: required, and must be greater than 0", at("video.bitrate.max_multiplier"))
	}
	if b.MinMultiplier > 0 && b.MaxMultiplier > 0 && b.MinMultiplier > b.MaxMultiplier {
		p.addf("%s: min_multiplier %v is above max_multiplier %v", at("video.bitrate"), b.MinMultiplier, b.MaxMultiplier)
	}

	// Absent is not the same as zero here; see bitrateYAML.FloorKbps.
	switch {
	case raw.FloorKbps == nil:
		p.addf("%s: required. It is the most important quality control in this file -- it is what makes the tool decline a file rather than compress it hard. Write `floor_kbps: 0` if you really want no floor", at("video.bitrate.floor_kbps"))
	case *raw.FloorKbps < 0:
		p.addf("%s: must not be negative", at("video.bitrate.floor_kbps"))
	}
}

func validateAudio(at func(string) string, a decide.AudioRules, p *problems) {
	if a.MultichannelTo == "" {
		p.addf("%s: required", at("audio.multichannel_to"))
	}
	if a.MultichannelThreshold < 2 {
		p.addf("%s: must be at least 2 (6 means 5.1 and above)", at("audio.multichannel_threshold"))
	}
	if len(a.KeepLanguages) == 0 {
		p.addf("%s: required; with no keep-list every tagged audio track would be dropped", at("audio.keep_languages"))
	}
	checkLanguages(at("audio.keep_languages"), a.KeepLanguages, p)
	checkLanguages(at("audio.primary_languages"), a.PrimaryLanguages, p)
	if a.TagUntaggedAs != "" {
		checkLanguages(at("audio.tag_untagged_as"), []string{a.TagUntaggedAs}, p)
	}
	checkPrimarySubset(at("audio"), a.PrimaryLanguages, a.KeepLanguages, p)
}

func validateSubtitles(at func(string) string, s decide.SubtitleRules, p *problems) {
	checkLanguages(at("subtitles.keep_languages"), s.KeepLanguages, p)
	checkLanguages(at("subtitles.primary_languages"), s.PrimaryLanguages, p)
	if s.TagUntaggedAs != "" {
		checkLanguages(at("subtitles.tag_untagged_as"), []string{s.TagUntaggedAs}, p)
	}
	if len(s.KeepLanguages) > 0 {
		checkPrimarySubset(at("subtitles"), s.PrimaryLanguages, s.KeepLanguages, p)
	}
}

// checkLanguages catches the typo that would otherwise never announce itself.
// A keep-list entry that matches nothing does not error, it just quietly stops
// keeping that language.
func checkLanguages(where string, langs []string, p *problems) {
	for _, l := range langs {
		if !languageTag.MatchString(l) {
			p.addf("%s: %q does not look like a language code (expected something like eng, jpn, und)", where, l)
		}
	}
}

// checkPrimarySubset catches a profile that contradicts itself: naming a
// language as one this library is expected to contain, while also dropping it.
func checkPrimarySubset(where string, primary, keep []string, p *problems) {
	for _, l := range primary {
		if !containsFold(keep, l) {
			p.addf("%s: primary_languages contains %q but keep_languages does not, so a track in the language this library is built around would be dropped", where, l)
		}
	}
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// isHardwareEncoder mirrors the check in decide: these encoders need a render
// node to talk to.
func isHardwareEncoder(encoder string) bool {
	return strings.HasSuffix(encoder, "_vaapi") || strings.HasSuffix(encoder, "_qsv")
}
