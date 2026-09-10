package encode

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// tempPath picks where the in-progress output goes, and reports whether the
// configured work dir was usable.
//
// The rule is entirely about the final step. Replacement has to be a rename(),
// because a rename is atomic: at no instant does the path hold half a file.
// rename() only works within one filesystem, so a work dir on a different
// filesystem is not a faster scratch space, it is a different algorithm --
// encode, copy the whole file back across, then delete. That is slower, not
// atomic, and needs twice the space at the destination anyway. So when the
// work dir is elsewhere, the temp file goes beside the source instead.
func (e Encoder) tempPath(src string, target decide.Container) (string, bool) {
	dir := filepath.Dir(src)
	name := tempName(src, target.Extension())
	if e.WorkDir != "" && sameFilesystem(e.WorkDir, dir) {
		return filepath.Join(e.WorkDir, name), true
	}
	return filepath.Join(dir, name), false
}

// tempName builds the temp file's name.
//
// Three things are being satisfied at once:
//
//   - It ends in the target container's extension, because that is how ffmpeg
//     chooses a muxer.
//   - It starts with a dot and contains ".partial.", so a leftover from a
//     crashed run is both hidden and caught by the scanner's default ignore
//     globs. A temp file that the next scan picks up as a new arrival would
//     be a genuinely nasty loop.
//   - It contains a hash of the full source path, because a shared work dir
//     collects files from every library and "S01E01.mkv" is not unique.
//
// It is deterministic rather than random so that a temp file left behind by a
// failure is findable from the source path alone.
func tempName(src, ext string) string {
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	sum := sha256.Sum256([]byte(src))
	return fmt.Sprintf(".%s.%x.mediacompressor.partial%s", truncate(base, 80), sum[:4], ext)
}

// truncate cuts a string to at most n bytes without splitting a rune, so that
// a name with non-ASCII characters in it stays valid UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// sameFilesystem reports whether two directories are on the same filesystem.
//
// When it cannot tell -- either directory missing, or a platform that does not
// report device numbers -- the answer is no, which routes the temp file beside
// the source. That is the conservative direction: it is always correct, just
// occasionally slower than it needed to be.
func sameFilesystem(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	adev, ok := deviceOf(ai)
	if !ok {
		return false
	}
	bdev, ok := deviceOf(bi)
	if !ok {
		return false
	}
	return adev == bdev
}
