//go:build !unix

package main

import "os"

// fileOwnerUIDPlatform has no ownership concept to report off Unix. A profile
// can therefore never be authenticated on such a host, which is correct: fresh
// native provisioning is a Linux appliance operation.
func fileOwnerUIDPlatform(os.FileInfo) (int, bool) {
	return 0, false
}

// provisioningProfileSupported reports whether this platform can carry a
// provisioning profile at all. It returns false off Unix so a host with no
// concept of a root-owned file simply has no profile, rather than evaluating a
// POSIX-shaped path under different semantics — a Windows host would otherwise
// treat the default path as non-absolute and fail configuration loading, which
// must never be the reason an ordinary node refuses to start.
func provisioningProfileSupported() bool { return false }
