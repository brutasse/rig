//go:build !windows

package jvm

import (
	"os"
	"syscall"
)

// exec replaces the current process image with r.Java, in r.Dir, with the
// process environment plus r.Env. On success it does not return.
func (r Run) exec() error {
	if r.Dir != "" {
		if err := os.Chdir(r.Dir); err != nil {
			return err
		}
	}
	return syscall.Exec(r.Java, append([]string{r.Java}, r.Args...), append(os.Environ(), r.Env...))
}
