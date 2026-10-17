//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileOwnerUIDPlatform reports the owning uid of a file on Unix hosts.
func fileOwnerUIDPlatform(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

// provisioningProfileSupported reports whether this platform can carry a
// provisioning profile at all. Fresh native provisioning is a Linux appliance
// operation.
func provisioningProfileSupported() bool { return true }
