package cli

import "fmt"

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func exitf(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

// exitCode propagates a child process's exit code verbatim (no message).
func exitCode(code int) error {
	return &exitError{code: code}
}
