package jvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// Code returns the child process exit code of err, or -1 when err is not a
// child exit (e.g. the binary could not start).
func Code(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

var versionRe = regexp.MustCompile(`version "([^"]+)"`)

func Find() (string, error) {
	if home := os.Getenv("JAVA_HOME"); home != "" {
		p := filepath.Join(home, "bin", "java")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("java")
	if err != nil {
		return "", fmt.Errorf("jvm: no java found (set JAVA_HOME or put java on PATH)")
	}
	return p, nil
}

func Version(java string) (string, error) {
	out, err := exec.Command(java, "-version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("jvm: %v: %s", err, out)
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("jvm: cannot parse java -version output: %s", out)
	}
	return string(m[1]), nil
}

type Run struct {
	Java string
	Args []string
	Dir  string
	Env  []string
	// Ctx bounds the forked JVM's lifetime: cancelling it kills the
	// process and Run returns (the Exec path hands the process to the
	// JVM, where the caller's own signal handling applies instead).
	Ctx context.Context
	// Exec, when set, replaces the current process with the JVM instead of
	// forking and waiting: the caller's process becomes the JVM (its exit
	// code is the JVM's, and signals reach it directly). On platforms
	// without process exec, or when exec fails, Run falls back to forking
	// and waiting.
	Exec bool
}

func (r Run) Run() error {
	if r.Exec {
		if err := r.exec(); err == nil {
			return nil // unreachable: the process image was replaced
		}
		// No process exec on this platform (or it failed): fork and wait.
	}
	var cmd *exec.Cmd
	if r.Ctx != nil {
		cmd = exec.CommandContext(r.Ctx, r.Java, r.Args...)
	} else {
		cmd = exec.Command(r.Java, r.Args...)
	}
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), r.Env...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
