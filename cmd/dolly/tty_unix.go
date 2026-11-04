//go:build linux || darwin

package main

import (
	"syscall"
	"unsafe"
)

func termEchoOff(fd uintptr, get, set uintptr) (func(), error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, get, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	t := old
	t.Lflag &^= syscall.ECHO
	t.Lflag |= syscall.ICANON | syscall.ECHONL // still show the newline
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, set, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, errno
	}
	return func() { syscall.Syscall(syscall.SYS_IOCTL, fd, set, uintptr(unsafe.Pointer(&old))) }, nil
}
