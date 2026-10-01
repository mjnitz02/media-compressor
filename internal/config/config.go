// Package config reads config.yaml and turns it into something the rest of
// the program can use without ever looking at YAML again.
//
// Two ideas are kept apart on purpose, and the whole package exists to serve
// the split:
//
//   - a *profile* is the work: what to encode to, which tracks to keep.
//   - a *library* is where: a name, a profile reference, and some paths.
//
// The stack this replaces bundled the two together, so a dozen folders wanting
// identical treatment meant a dozen duplicated plugin stacks. Here, adding a
// folder is one line of YAML. See docs/libraries.md.
//
// Parsing is deliberately strict. An unknown key, a profile name that does not
// resolve, or a bitrate tier table in the wrong order is a startup error with
// a message naming the exact place -- never a silent fallback to different
// encode settings, because the failure mode of that is a library quietly
// encoded with the wrong quality target and no way to notice.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"gopkg.in/yaml.v3"
)

// Config is a parsed, resolved, validated configuration. Every profile
// reference has been followed and every default applied, so nothing
// downstream has to ask "was this set?".
type Config struct {
	Defaults  Defaults
	Libraries []Library
	Scanner   Scanner
	Paths     Paths
	Server    Server

	// Profiles are the resolved profiles by name, before any per-library
	// container override. Libraries carry their own finished copy; this is
	// here so `validate` can print the set and so a profile defined but never
	// used can be reported.
	Profiles map[string]decide.Profile

	// Warnings are things that are legal but worth seeing before a run --
	// most importantly a library whose container setting will rename files.
	// They never block startup.
	Warnings []string
}

// Library is one place to work, with its profile already resolved and its
// container override already folded in.
type Library struct {
	Name        string
	ProfileName string
	Paths       []string
	NotifyPlex  bool

	// Profile is the finished profile for this library: the named profile
	// with the library's container override applied.
	Profile decide.Profile
}

// Defaults apply to every library that does not say otherwise.
type Defaults struct {
	Profile   string  `yaml:"profile"`
	Container string  `yaml:"container"`
	Workers   Workers `yaml:"workers"`
}

// Workers splits concurrency by what the work contends for. Remuxes are I/O
// bound and several can run at once; hardware encoding is bottlenecked on one
// iGPU and realistically supports one or two.
type Workers struct {
	Remux  int `yaml:"remux"`
	Encode int `yaml:"encode"`
}

// Scanner controls which files are considered and when they are eligible.
type Scanner struct {
	Extensions  []string `yaml:"extensions"`
	IgnoreGlobs []string `yaml:"ignore_globs"`

	// MinAgeSeconds leaves freshly written files alone until they have
	// settled. Age alone is not sufficient on this server -- a file can be
	// older than the grace period and still be mid-move -- so Phase 4 also
	// requires a size that has not changed between two scans.
	MinAgeSeconds   int `yaml:"min_age_seconds"`
	IntervalMinutes int `yaml:"interval_minutes"`
}

// Paths are the two directories the program itself writes to. Media paths
// live on libraries, not here.
type Paths struct {
	WorkDir  string `yaml:"work_dir"`
	Database string `yaml:"database"`
}

// Server is the web UI listen address.
type Server struct {
	Listen string `yaml:"listen"`
}

// file mirrors the YAML document exactly. It is separate from Config because
// the document is full of "unset means inherit", and Config is the version
// where every question already has an answer.
type file struct {
	Defaults Defaults `yaml:"defaults"`

	// Profiles stay as raw nodes until resolveProfiles walks them, because
	// `extends` means a profile cannot be decoded until its parent has been.
	Profiles map[string]yaml.Node `yaml:"profiles"`

	Libraries []rawLibrary `yaml:"libraries"`
	Scanner   Scanner      `yaml:"scanner"`
	Paths     Paths        `yaml:"paths"`
	Server    Server       `yaml:"server"`
}

// rawLibrary is a library as written. The pointer fields are the ones where
// "absent" and "set to the zero value" mean different things: a nil Container
// inherits the default, an empty one is a typo worth reporting, and
// notify_plex has to be able to say false without that meaning "unset".
type rawLibrary struct {
	Name       string   `yaml:"name"`
	Profile    string   `yaml:"profile"`
	Container  *string  `yaml:"container"`
	NotifyPlex *bool    `yaml:"notify_plex"`
	Paths      []string `yaml:"paths"`
}

// DefaultListen is where the web UI goes when server.listen says nothing. It
// is named because the daemon needs an address before it has a config to read
// one from -- see waitForConfig.
const DefaultListen = ":8080"

// Load reads and parses a config file.
//
// It does not touch the media paths; that is CheckPaths, kept separate so
// that everything except "does this directory exist" can be tested and
// validated without a filesystem.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		// Put the filename on the error, since the parse errors below are
		// all about positions inside the document.
		var ce *Error
		if errors.As(err, &ce) {
			ce.Source = path
			return nil, ce
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse turns a YAML document into a finished Config.
func Parse(data []byte) (*Config, error) {
	var f file

	// KnownFields makes an unrecognised key an error rather than something
	// silently ignored. A misspelled `keep_langauges` that quietly did
	// nothing would drop tracks the operator meant to keep.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, &Error{Problems: []string{yamlProblem(err)}}
	}

	c := &Config{
		Defaults: f.Defaults,
		Scanner:  f.Scanner,
		Paths:    f.Paths,
		Server:   f.Server,
	}
	applyBuiltinDefaults(c)

	var p problems
	c.Profiles = resolveProfiles(f.Profiles, c.Defaults.Container, &p)
	c.Libraries = resolveLibraries(f.Libraries, c, &p)
	validate(c, f, &p)

	if err := p.err(); err != nil {
		return nil, err
	}
	c.Warnings = warningsFor(c)
	return c, nil
}

// applyBuiltinDefaults fills in the few values that have an obviously correct
// answer and no quality implications. Anything that affects what a file ends
// up looking like is required in the document instead -- see validate.
func applyBuiltinDefaults(c *Config) {
	if c.Defaults.Profile == "" {
		c.Defaults.Profile = "standard"
	}
	if c.Defaults.Container == "" {
		c.Defaults.Container = string(decide.ContainerMKV)
	}
	if c.Defaults.Workers.Remux == 0 {
		c.Defaults.Workers.Remux = 3
	}
	if c.Defaults.Workers.Encode == 0 {
		c.Defaults.Workers.Encode = 1
	}
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Scanner.IntervalMinutes == 0 {
		c.Scanner.IntervalMinutes = 180
	}
}

// resolveLibraries applies the defaults to each library and attaches its
// finished profile.
func resolveLibraries(raw []rawLibrary, c *Config, p *problems) []Library {
	libs := make([]Library, 0, len(raw))
	for i, r := range raw {
		where := fmt.Sprintf("libraries[%d]", i)
		if r.Name != "" {
			where = "library " + r.Name
		}

		lib := Library{
			Name:        r.Name,
			ProfileName: r.Profile,
			Paths:       r.Paths,
			NotifyPlex:  true,
		}
		if lib.ProfileName == "" {
			lib.ProfileName = c.Defaults.Profile
		}
		if r.NotifyPlex != nil {
			lib.NotifyPlex = *r.NotifyPlex
		}

		prof, ok := c.Profiles[lib.ProfileName]
		if !ok {
			// The single most important error in this file. A typo'd profile
			// name must never fall back to some other encode settings.
			p.addf("%s: profile %q is not defined (have: %s)", where, lib.ProfileName, nameList(c.Profiles))
		} else {
			lib.Profile = prof
		}

		// Container precedence: the library wins over the profile, which wins
		// over the file defaults. Only the library override is checked here;
		// the other two are checked where they are written.
		if r.Container != nil {
			if !validContainer(*r.Container) {
				p.addf("%s: container %q is not one of mkv, mp4, source", where, *r.Container)
			} else {
				lib.Profile.Container = decide.Container(*r.Container)
			}
		}
		libs = append(libs, lib)
	}
	return libs
}

// CheckPaths reports library paths that are missing or are not directories.
//
// Kept out of Parse because it is the one part of configuration that needs
// the world: `validate` on a laptop should still be able to check everything
// else about a config for a server whose mounts are not present.
func (c *Config) CheckPaths() error {
	var p problems
	for _, lib := range c.Libraries {
		for _, path := range lib.Paths {
			info, err := os.Stat(path)
			switch {
			case err != nil:
				p.addf("library %s: path %s is not readable: %v", lib.Name, path, err)
			case !info.IsDir():
				p.addf("library %s: path %s is not a directory", lib.Name, path)
			}
		}
	}
	if c.Paths.WorkDir != "" {
		if info, err := os.Stat(c.Paths.WorkDir); err != nil || !info.IsDir() {
			p.addf("paths.work_dir: %s is not an existing directory", c.Paths.WorkDir)
		}
	}
	if dir := filepath.Dir(c.Paths.Database); dir != "" && dir != "." {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			p.addf("paths.database: directory %s does not exist", dir)
		}
	}
	return p.err()
}

// warningsFor collects things that are legal but that you would want to know
// before a run, not discover afterwards.
func warningsFor(c *Config) []string {
	var w []string
	used := map[string]bool{}
	for _, lib := range c.Libraries {
		used[lib.ProfileName] = true

		// Forcing MP4 is the one container setting that can cost you a
		// stream, so it is worth saying out loud. Forcing MKV is the normal
		// case and only renames a file that was MP4 to begin with, which
		// --dry-run reports per file in Phase 3; warning about it here would
		// fire on almost every library and train you to ignore warnings.
		if lib.Profile.Container == decide.ContainerMP4 {
			w = append(w, fmt.Sprintf("library %s forces mp4: every non-mp4 file in it will be renamed, image-based subtitles (PGS, VOBSUB) will be dropped, and text subtitles converted to mov_text", lib.Name))
		}
	}
	for name := range c.Profiles {
		if !used[name] {
			w = append(w, fmt.Sprintf("profile %s is defined but no library uses it", name))
		}
	}
	sortStrings(w)
	return w
}

func validContainer(s string) bool {
	switch decide.Container(s) {
	case decide.ContainerMKV, decide.ContainerMP4, decide.ContainerSource:
		return true
	}
	return false
}

func nameList(m map[string]decide.Profile) string {
	if len(m) == 0 {
		return "none defined"
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

// LibraryFor returns the library whose configured paths contain path, so that
// a file found on disk can be matched back to the profile that governs it.
//
// Validation guarantees no two libraries have overlapping paths, so at most
// one can match and the answer does not depend on the order libraries were
// written in. The longest matching path wins anyway, which keeps this correct
// if that rule is ever relaxed.
func (c *Config) LibraryFor(path string) (*Library, bool) {
	path = filepath.Clean(path)
	best := -1
	bestLen := -1
	for i := range c.Libraries {
		for _, root := range c.Libraries[i].Paths {
			root = filepath.Clean(root)
			if !under(path, root) {
				continue
			}
			if len(root) > bestLen {
				best, bestLen = i, len(root)
			}
		}
	}
	if best < 0 {
		return nil, false
	}
	return &c.Libraries[best], true
}

// under reports whether path is root or is inside it. The separator check is
// what stops /mnt/media_video/tv matching /mnt/media_video/tv_animated.
func under(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}
