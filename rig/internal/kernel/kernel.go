package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/digest"
)

var (
	ErrResolution = errors.New("kernel: resolution failed")
	ErrBadRequest = errors.New("kernel: bad request")
	ErrLegacyKeys = errors.New("kernel: manifest still uses legacy keys - run rig migrate")
)

type Pin struct {
	Lib     string
	Version string
	GitSHA  string
	URL     string
	JARSHA  string
}

var Current = Pin{
	Lib:     "io.github.brutasse/rig-resolver",
	Version: "v0.1.0",
	GitSHA:  "f705018de3631a52fc6942a428c7a355951a3fa7",
	URL:     "https://github.com/brutasse/rig/releases/download/0.1.0/rig-resolver-0.1.0.jar",
	// SHA256 of the release kernel jar; 'make pin V=…' stamps this whole
	// block (version, git sha, URL, jar sha) per release. Local builds:
	// RIG_KERNEL_JAR must match JARSHA.
	JARSHA: "ce65538583a29fcbcaa827cd659bb9b95a0f193d828b93c45a7b53b5fefdf27c",
}

type Request struct {
	Op        string         `json:"op"`
	Workspace string         `json:"workspace"`
	Modules   []string       `json:"modules,omitempty"`
	Lock      string         `json:"lock,omitempty"`
	Args      map[string]any `json:"args"`
}

func (p Pin) Ensure(ctx context.Context, store *cache.Store, offline bool) (string, error) {
	if local := os.Getenv("RIG_KERNEL_JAR"); local != "" {
		got, err := digest.File(local)
		if err != nil {
			return "", err
		}
		if got != p.JARSHA {
			return "", fmt.Errorf("kernel: %s: sha256 mismatch (got %s)", local, got)
		}
		return local, nil
	}
	jar := store.KernelJar(p.GitSHA)
	if st, err := os.Stat(jar); err == nil && !st.IsDir() {
		got, err := digest.File(jar)
		if err != nil {
			return "", err
		}
		if got != p.JARSHA {
			return "", fmt.Errorf("kernel: cached jar %s corrupted (sha256 mismatch)", jar)
		}
		return jar, nil
	}
	if offline {
		return "", fmt.Errorf("offline: kernel jar %s not in cache (run 'rig lock' online once)", p.Lib)
	}
	if err := os.MkdirAll(store.KernelDir(p.GitSHA), 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kernel: %s: status %d", p.URL, resp.StatusCode)
	}
	tmp := jar + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	got, err := digest.File(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if got != p.JARSHA {
		os.Remove(tmp)
		return "", fmt.Errorf("kernel: %s: sha256 mismatch (got %s)", p.URL, got)
	}
	if err := os.Rename(tmp, jar); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return jar, nil
}

type OpError struct {
	Op  string
	Err error
}

func (e *OpError) Error() string { return e.Op + ": " + e.Err.Error() }

func (e *OpError) Unwrap() error { return e.Err }

// lastLine returns the final line of b. The kernel protocol is one JSON
// document printed last on stdout; ops that spawn subprocesses (build AOT
// compilation) may emit noise before it.
func lastLine(b []byte) []byte {
	b = bytes.TrimRight(b, "\n")
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return b[i+1:]
	}
	return b
}

// Call runs one kernel op. env, when non-empty, is appended to the
// inherited environment of the kernel JVM (e.g. RIG_TOKEN for the
// resolver's authenticated repository probes).
func Call(ctx context.Context, jar, java string, req Request, env ...string) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, java, "-jar", jar, "--request", "-")
	// tools.deps refuses http:// custom repos unless this is set; the kernel
	// serves :mvn/repos as rig defines them (local dev and E2E servers are
	// http), so the kernel JVM always allows them.
	cmd.Env = append(os.Environ(), "CLOJURE_CLI_ALLOW_HTTP_REPO=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if err == nil {
		return lastLine(out.Bytes()), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		var mapped error
		switch ee.ExitCode() {
		case 1:
			mapped = ErrResolution
		case 2:
			mapped = ErrBadRequest
		case 3:
			mapped = ErrLegacyKeys
		}
		if mapped != nil {
			return out.Bytes(), &OpError{Op: req.Op, Err: mapped}
		}
	}
	return out.Bytes(), err
}
