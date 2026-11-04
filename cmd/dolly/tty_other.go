//go:build !linux && !darwin

package main

import "errors"

// isTerminal is false where Dolly can't tell: dolly unlock then needs --yes,
// and dolly config refuses to run.
func isTerminal(uintptr) bool { return false }

func echoOff(uintptr) (func(), error) { return nil, errors.New("no terminal support on this system") }
