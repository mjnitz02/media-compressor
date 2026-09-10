package decide

import "strings"

// argList builds an ffmpeg argument list in append order.
//
// Order matters more than it should: the golden corpus records the exact
// command the old stack produced, and asserting against it is only possible if
// the arguments come out in the same sequence. So this mirrors the structure of
// the plugin it replaces -- a list of default settings, some of which get
// removed and replaced as decisions are made.
type argList struct {
	entries [][]string
	changed bool // the ffmpeg equivalent of "there is work to do here"
}

// preset adds an argument that does not on its own mean the file needs
// processing, e.g. `-c:v copy`.
func (a *argList) preset(tokens ...string) {
	a.entries = append(a.entries, tokens)
}

// add adds an argument that does mean the file needs processing.
func (a *argList) add(tokens ...string) {
	a.entries = append(a.entries, tokens)
	a.changed = true
}

// remove deletes the first entry matching tokens exactly, and reports whether
// it found one. Used to take out a default `-c:v copy` before adding a real
// encoder.
func (a *argList) remove(tokens ...string) bool {
	for i, e := range a.entries {
		if equalTokens(e, tokens) {
			a.entries = append(a.entries[:i], a.entries[i+1:]...)
			return true
		}
	}
	return false
}

// has reports whether an entry matching tokens is already present.
func (a *argList) has(tokens ...string) bool {
	for _, e := range a.entries {
		if equalTokens(e, tokens) {
			return true
		}
	}
	return false
}

// flat returns the arguments as a single token slice, ready for os/exec.
func (a *argList) flat() []string {
	var out []string
	for _, e := range a.entries {
		out = append(out, e...)
	}
	return out
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsFold reports whether s contains any of the needles,
// case-insensitively. Used for the commentary/description/SDH title match.
func containsFold(s string, needles []string) (string, bool) {
	if s == "" {
		return "", false
	}
	low := strings.ToLower(s)
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(low, strings.ToLower(n)) {
			return n, true
		}
	}
	return "", false
}

// inList reports whether v is in list, case-insensitively.
func inList(v string, list []string) bool {
	for _, x := range list {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}
