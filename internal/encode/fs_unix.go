//go:build unix

package encode

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// deviceOf returns the filesystem device a file lives on.
func deviceOf(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	// Dev is int32 on darwin and uint64 on linux; the value is only ever
	// compared with another one from the same call, so the conversion is
	// safe on both.
	return uint64(st.Dev), true
}

// linkCount returns how many directory entries point at this file. More than
// one means an *arr stack (or anything else) hardlinked it, and replacing this
// path leaves the other link holding the old data.
func linkCount(info fs.FileInfo) uint64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 1
	}
	return uint64(st.Nlink)
}

// adoptOwnership gives the temp file the mode and owner of the file it is
// about to replace, and returns a note if it could not.
//
// Without this, every processed file silently takes on the container's umask
// and uid. On an Unraid box where Plex, the *arr stacks and SMB all read the
// same share, a file that turns up as 0600 root:root is a support call.
//
// Neither failure is worth abandoning a verified encode over -- the file is
// correct, only its metadata is not -- so both are reported rather than
// returned as errors.
func adoptOwnership(path string, src fs.FileInfo) string {
	if err := os.Chmod(path, src.Mode().Perm()); err != nil {
		return fmt.Sprintf("could not set permissions %v on the replacement: %v", src.Mode().Perm(), err)
	}
	st, ok := src.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil {
		// Expected when not running as root, which is why it is only a note.
		return fmt.Sprintf("could not set owner %d:%d on the replacement: %v", st.Uid, st.Gid, err)
	}
	return ""
}
