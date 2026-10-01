//go:build windows

package jvm

import "errors"

// exec is unavailable on Windows; Run falls back to forking and waiting.
func (r Run) exec() error { return errors.New("jvm: process exec is not supported on this platform") }
