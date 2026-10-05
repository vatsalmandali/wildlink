//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// freeSpace returns the bytes available on the volume holding dir (0 if unknown).
func freeSpace(dir string) int64 {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0
	}
	var free, total, totalFree uint64
	r, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW").Call(
		uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0
	}
	return int64(free)
}
