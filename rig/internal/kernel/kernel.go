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
	"path/filepath"
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
	// RunnerURL/RunnerSHA pin the rig.runner jar — an install-time
	// artifact hot commands append to the project's locked classpath
	// (the kernel jar itself must not sit on it: its bundled
	// dependencies would load in place of anything a module forgot to
	// declare, a false green check). Stamped by the first release that
	// ships rig-runner (v0.1.0 predates it); make pin fills both.
	RunnerURL string
	RunnerSHA string
}

// Current is the last released kernel. The release workflow stamps this
// block (version, git sha, URL, jar sha256, runner URL, runner jar
// sha256) per release via 'make pin' and pushes it to main; feature
// branches must not commit pin changes. Local development overrides the
// kernel with RIG_KERNEL_JAR and the runner with RIG_RUNNER_JAR
// (trusted, not hash-checked).
var Current = Pin{
	Lib:       "io.github.brutasse/rig-resolver",
	Version:   "v0.1.0",
	GitSHA:    "29b0bb60817c7ce2db5d3a2775a3b64859f214f5",
	URL:       "https://github.com/brutasse/rig/releases/download/v0.1.0/rig-resolver-v0.1.0.jar",
	JARSHA:    "823c7b174dfe01874cb2ae74790d51f0f293ecf9f592acb76223ed3424208934",
	RunnerURL: "",
	RunnerSHA: "",
}

type Request struct {
	Op        string         `json:"op"`
	Workspace string         `json:"workspace"`
	Modules   []string       `json:"modules,omitempty"`
	Lock      string         `json:"lock,omitempty"`
	Args      map[string]any `json:"args"`
}

// localOverride returns the artifact the developer pointed at via env,
// used as-is. The sha gates cover what rig fetches and caches, not an
// explicit local choice.
func localOverride(env string) (string, error) {
	local := os.Getenv(env)
	if local == "" {
		return "", nil
	}
	if st, err := os.Stat(local); err != nil || st.IsDir() {
		return "", fmt.Errorf("kernel: %s: %s: not a file", env, local)
	}
	return local, nil
}

func (p Pin) Ensure(ctx context.Context, store *cache.Store, offline bool) (string, error) {
	if local, err := localOverride("RIG_KERNEL_JAR"); err != nil {
		return "", err
	} else if local != "" {
		return local, nil
	}
	return ensureCached(ctx, offline,
		fmt.Errorf("offline: kernel jar %s not in cache (run 'rig lock' online once)", p.Lib),
		p.URL, p.JARSHA, store.KernelJar(p.GitSHA))
}

// Runner returns the on-disk path of the pin's rig.runner jar. The runner
// is an install-time artifact: pre-seeded stores (the Docker image, a
// shared read-only cache) need no writes, and rig never extracts it at
// runtime. Hot commands append the jar to the project's locked classpath
// (never the kernel jar: its bundled dependencies would load in place of
// anything a module forgot to declare, a false green check).
func (p Pin) Runner(ctx context.Context, store *cache.Store, offline bool) (string, error) {
	if local, err := localOverride("RIG_RUNNER_JAR"); err != nil {
		return "", err
	} else if local != "" {
		return local, nil
	}
	if os.Getenv("RIG_KERNEL_JAR") != "" {
		return "", fmt.Errorf("kernel: RIG_KERNEL_JAR overrides the kernel; set RIG_RUNNER_JAR to a matching runner jar (make kernel builds both)")
	}
	if p.RunnerSHA == "" {
		return "", fmt.Errorf("kernel: %s pins no runner artifact; set RIG_RUNNER_JAR or upgrade rig", p.Version)
	}
	return ensureCached(ctx, offline,
		fmt.Errorf("offline: runner jar not in cache (run 'rig check' online once)"),
		p.RunnerURL, p.RunnerSHA, store.RunnerJar(p.GitSHA))
}

// ensureCached returns storePath holding the artifact served at url: the
// cached copy, sha256-verified, when present, else a fresh download,
// verified before it lands (a per-path temp, so a crashed run never
// leaves a partial artifact). offlineErr is returned when the artifact
// is missing and the network is refused.
func ensureCached(ctx context.Context, offline bool, offlineErr error, url, sha, storePath string) (string, error) {
	if st, err := os.Stat(storePath); err == nil && !st.IsDir() {
		got, err := digest.File(storePath)
		if err != nil {
			return "", err
		}
		if got != sha {
			return "", fmt.Errorf("kernel: cached %s corrupted (sha256 mismatch)", filepath.Base(storePath))
		}
		return storePath, nil
	}
	if offline {
		return "", offlineErr
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return "", fmt.Errorf("kernel: %s: status %d", url, resp.StatusCode)
	}
	tmp := storePath + ".tmp"
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
	if got != sha {
		os.Remove(tmp)
		return "", fmt.Errorf("kernel: %s: sha256 mismatch (got %s)", url, got)
	}
	if err := os.Rename(tmp, storePath); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return storePath, nil
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
