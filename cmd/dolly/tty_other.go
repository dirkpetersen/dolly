//go:build !linux && !darwin

package main

// isTerminal is false where Dolly can't tell: dolly unlock then needs --yes.
func isTerminal(uintptr) bool { return false }
