package config

import (
	"fmt"
	"sort"
	"strings"
)

// Error is every problem found in one configuration file, reported together.
//
// Reporting them together is deliberate. Fixing a config one error per run is
// miserable, and the operator opens this file about twice a year, so the goal
// is that a single `validate` tells you everything that is wrong.
type Error struct {
	Source   string // filename, when it was read from one
	Problems []string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Source != "" {
		fmt.Fprintf(&b, "%s: ", e.Source)
	}
	if len(e.Problems) == 1 {
		b.WriteString(e.Problems[0])
		return b.String()
	}
	fmt.Fprintf(&b, "%d problems:", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  - %s", p)
	}
	return b.String()
}

// problems accumulates messages so that validation can keep going after the
// first thing it finds.
type problems struct{ list []string }

func (p *problems) addf(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (p *problems) err() error {
	if len(p.list) == 0 {
		return nil
	}
	return &Error{Problems: p.list}
}

// yamlProblem tidies up the decoder's own errors. yaml.v3 prefixes multi-error
// output with "yaml: unmarshal errors:" and indents each line, which reads
// badly once it is nested inside our own list.
func yamlProblem(err error) string {
	msg := strings.TrimPrefix(err.Error(), "yaml: unmarshal errors:\n")
	lines := strings.Split(msg, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "; ")
}

func sortStrings(s []string) { sort.Strings(s) }
