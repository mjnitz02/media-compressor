//go:build !unix

package encode

import (
	"io/fs"
	"os"
)

// deviceOf cannot answer on a platform without stat device numbers, so the
// work dir is simply never used. See sameFilesystem.
func deviceOf(fs.FileInfo) (uint64, bool) { return 0, false }

func linkCount(fs.FileInfo) uint64 { return 1 }

func adoptOwnership(path string, src fs.FileInfo) string {
	if err := os.Chmod(path, src.Mode().Perm()); err != nil {
		return "could not set permissions on the replacement: " + err.Error()
	}
	return ""
}
