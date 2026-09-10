package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// DefaultBinary is the ffprobe to use when a config does not name one.
const DefaultBinary = "ffprobe"

// Prober runs ffprobe. It is the only part of probing that touches the world,
// which is why it is separate from the types above: decide takes a *Result and
// never knows this exists.
type Prober struct {
	// Binary is the ffprobe executable. Empty means DefaultBinary.
	Binary string
}

// Run probes one file.
//
// It never modifies the file: -show_format and -show_streams are read-only, and
// the path is passed as a single argument rather than through a shell, so
// names with spaces, quotes or leading dashes cannot be misread as options.
func (p Prober) Run(ctx context.Context, path string) (*Result, error) {
	bin := p.Binary
	if bin == "" {
		bin = DefaultBinary
	}

	cmd := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		// "--" would be ideal here, but ffprobe does not accept it; the input
		// is positional and always last.
		path,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffprobe %s: %s", path, msg)
	}

	var r Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return nil, fmt.Errorf("ffprobe %s: parsing output: %w", path, err)
	}
	r.Path = path

	if len(r.Streams) == 0 {
		return nil, fmt.Errorf("ffprobe %s: no streams reported", path)
	}
	return &r, nil
}
