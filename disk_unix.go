//go:build !windows

package main

import "syscall"

// freeSpace returns the bytes available on the volume holding dir (0 if unknown).
func freeSpace(dir string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize))
}
