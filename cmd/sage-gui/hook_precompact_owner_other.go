//go:build !unix

package main

import "os"

// fileOwnedByCurrentUser is a no-op on non-unix platforms; the trusted-root,
// symlink, and regular-file checks still apply there.
func fileOwnedByCurrentUser(_ os.FileInfo) error { return nil }
