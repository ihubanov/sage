//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// fileOwnedByCurrentUser rejects a transcript owned by a different uid, so the
// capture never reads a file planted by another user. Unix only.
func fileOwnedByCurrentUser(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // ownership indeterminable — best effort, don't block capture
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("transcript is owned by uid %d, not the current user (uid %d); refusing", st.Uid, os.Getuid())
	}
	return nil
}
