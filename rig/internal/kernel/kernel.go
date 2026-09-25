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

// Current is the last released kernel. The release workflow stamps this
// block (version, git sha, URL, jar sha256) per release via 'make pin' and
// pushes it to main; feature branches must not commit pin changes. Local
// development overrides the kernel with RIG_KERNEL_JAR (trusted, not
// hash-checked).
var Current = Pin{
	Lib:     "io.github.brutasse/rig-resolver",
	Version: "v0.1.0",
	GitSHA:  "29b0bb60817c7ce2db5d3a2775a3b64859f214f5",
	URL:     "https://github.com/brutasse/rig/releases/download/v0.1.0/rig-resolver-v0.1.0.jar",
	JARSHA:  "823c7b174dfe01874cb2ae74790d51f0f293ecf9f592acb76223ed3424208934",
}

type Request struct {
	Op        string         `json:"op"`
	Workspace string         `json:"workspace"`
	Modules   []string       `json:"modules,omitempty"`
	Lock      string         `json:"lock,omitempty"`
	Args      map[string]any `json:"args"`
}

func (p Pin) Ensure(ctx context.Context, store *cache.Store, offline bool) (string, error) {
	// Local override: the jar the developer pointed at is used as-is. The
	// JARSHA gate covers what rig fetches and caches, not an explicit local
	// choice.
	if local := os.Getenv("RIG_KERNEL_JAR"); local != "" {
		if st, err := os.Stat(local); err != nil || st.IsDir() {
			return "", fmt.Errorf("kernel: RIG_KERNEL_JAR: %s: not a file", local)
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
