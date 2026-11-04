package main

import (
	"syscall"
	"unsafe"
)

// isTerminal reports whether fd is a terminal (TCGETS succeeds). A plain
// character device check would also accept /dev/null.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// echoOff turns off echo on terminal fd and returns the function that
// restores the previous settings.
func echoOff(fd uintptr) (func(), error) {
	return termEchoOff(fd, syscall.TCGETS, syscall.TCSETS)
}
