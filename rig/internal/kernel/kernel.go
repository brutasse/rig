package kernel

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/kernelrun"
)

var (
	// ErrOpFailed is the mapping of the kernel's exit 1: the op failed,
	// whatever it was. The kernel prints the real error on stderr before
	// exiting; a trailer naming the failure (resolution, compile, …)
	// would claim more than the exit code carries.
	ErrOpFailed   = errors.New("kernel: op failed")
	ErrBadRequest = errors.New("kernel: bad request")
	ErrLegacyKeys = errors.New("kernel: manifest still uses legacy keys - run rig migrate")
)

// The jars ship inside the rig binary. The kernel uberjar and the runner
// jar are copied into embed/ by the Makefile from the local kernel build
// ('make kernel', then 'make rig' embeds them); until a build fills them,
// the committed files are 0-byte placeholders and every kernel op fails
// with a hint. The runner jar is what hot commands append to the
// project's locked classpath (the kernel jar itself must never sit on
// one: its bundled dependencies would load in place of anything a module
// forgot to declare, a false green check). The nREPL jar is vendored —
// one upstream jar whose only dependency (org.clojure/clojure) is
// provided: the project's own locked Clojure serves it. Version bumps are
// ordinary commits: replace the jar, rename the embed.
//
// Lib/Version/GitSHA are the embedded kernel's identity, stamped at
// release build time via -ldflags like cli.Version; local builds report
// "dev". Local development overrides the kernel with RIG_KERNEL_JAR and
// the runner with RIG_RUNNER_JAR (trusted, not hash-checked).
var (
	Lib     = "io.github.brutasse/rig-resolver"
	Version = "dev"
	GitSHA  = "dev"
)

//go:embed embed/rig-resolver.jar
var resolverJar []byte

//go:embed embed/rig-runner.jar
var runnerJar []byte

//go:embed embed/nrepl-1.8.0.jar
var nreplJar []byte

// NreplVersion is the vendored nREPL version, for diagnostics.
const NreplVersion = "1.8.0"

// Kernel returns the on-disk path of the embedded kernel jar, extracted
// into the store on first use. RIG_KERNEL_JAR overrides it (trusted, not
// hash-checked) for local development.
func Kernel(store *cache.Store) (string, error) {
	if local, err := localOverride("RIG_KERNEL_JAR"); err != nil {
		return "", err
	} else if local != "" {
		return local, nil
	}
	return materialize("kernel", resolverJar, store.KernelJar)
}

// Runner returns the embedded rig.runner jar, the artifact hot commands
// append to the project's locked classpath. RIG_KERNEL_JAR overrides the
// kernel with a jar the pinned runner was not built beside, so a kernel
// override requires a RIG_RUNNER_JAR override too (make kernel builds
// both from the same tree).
func Runner(store *cache.Store) (string, error) {
	if os.Getenv("RIG_KERNEL_JAR") != "" && os.Getenv("RIG_RUNNER_JAR") == "" {
		return "", fmt.Errorf("kernel: RIG_KERNEL_JAR overrides the kernel; set RIG_RUNNER_JAR to a matching runner jar (make kernel builds both)")
	}
	if local, err := localOverride("RIG_RUNNER_JAR"); err != nil {
		return "", err
	} else if local != "" {
		return local, nil
	}
	return materialize("runner", runnerJar, store.RunnerJar)
}

// Nrepl returns the embedded nREPL jar, the tool jar rig nrepl appends
// (last) to the project's locked classpath. It has no dev override: it is
// a vendored artifact, identical to the source tree it ships with.
func Nrepl(store *cache.Store) (string, error) {
	return materialize("nrepl", nreplJar, store.NreplJar)
}

// materialize writes an embedded jar to its content-sha path in the store
// and returns that path: a cached copy is adopted when its digest matches
// (it cannot mismatch by construction — the path names the digest — but a
// corrupted file is rewritten), else the bytes land through a unique temp
// file. An empty embedded jar is the committed placeholder of a tree that
// never ran 'make kernel'.
func materialize(name string, b []byte, at func(sha string) string) (string, error) {
	if len(b) == 0 {
		return "", fmt.Errorf("kernel: the embedded %s jar is a placeholder: build it with 'make kernel', then 'make rig' (or set RIG_KERNEL_JAR/RIG_RUNNER_JAR)", name)
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	p := at(sha)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		if got, err := digest.File(p); err == nil && got == sha {
			return p, nil
		}
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	// The path is content-addressed, so a losing extract race already
	// holds the same verified bytes.
	if err := os.Rename(tmp.Name(), p); err != nil {
		if _, serr := os.Stat(p); serr != nil {
			os.Remove(tmp.Name())
			return "", err
		}
	}
	return p, nil
}

type Request struct {
	Op        string         `json:"op"`
	Workspace string         `json:"workspace"`
	Modules   []string       `json:"modules,omitempty"`
	Lock      string         `json:"lock,omitempty"`
	Args      map[string]any `json:"args"`
	// JVMFlags are flags rig passes to the kernel JVM itself (before -jar),
	// from the workspace's :rig/compile-jvm-opts. Only ops that compile or
	// load the workspace's code set them; the kernel ignores the field.
	JVMFlags []string `json:"jvm-flags,omitempty"`
	// Env mirrors the request-configuring entries of the call environment
	// (see requestEnvKeys) into the request JSON. System/getenv is
	// boot-cached inside a JVM, so a kernel restored from a CRaC checkpoint
	// reads its bootstrap run's environment; the request copy is the fresh,
	// authoritative one there. The variables are ALSO still passed as
	// process env: pinned kernels predating the request channel read only
	// the environment, and the kernel's subprocesses (tools.deps git)
	// inherit it.
	Env map[string]string `json:"env,omitempty"`
}

// requestEnvKeys are the variable names the kernel reads via
// System/getenv during an op (rig.resolver.versions); Call copies matching
// entries of its env argument into Request.Env.
var requestEnvKeys = []string{"RIG_REPO_TOKENS", "RIG_PROXY_REPOS"}

func withRequestEnv(req Request, env []string) Request {
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || v == "" {
			continue
		}
		for _, want := range requestEnvKeys {
			if k == want {
				if req.Env == nil {
					req.Env = map[string]string{}
				}
				req.Env[k] = v
			}
		}
	}
	return req
}

// localOverride returns the artifact the developer pointed at via env,
// used as-is: the sha gates cover the embedded jars, not an explicit
// local choice.
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

type OpError struct {
	Op  string
	Err error
}

// Error renders just the wrapped error: every caller prints the verb
// itself ("build:", "lock:", …), so prefixing the kernel op name here
// would stutter ("build: build: kernel: op failed"). Op stays on the
// struct for programs that want the kernel-side op name.
func (e *OpError) Error() string { return e.Err.Error() }

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

// exitErr maps the kernel's op exit codes to sentinel errors; 0 and
// anything uninterpreted map to nil (the caller reports the raw code).
func exitErr(code int) error {
	switch code {
	case 1:
		return ErrOpFailed
	case 2:
		return ErrBadRequest
	case 3:
		return ErrLegacyKeys
	}
	return nil
}

// Call runs one kernel op. env, when non-empty, is appended to the
// inherited environment of the kernel JVM (e.g. RIG_TOKEN for the
// resolver's authenticated repository probes).
//
// Eligible ops first attempt a run on the CRaC checkpoint of a warmed
// kernel (internal/kernelrun): a restored kernel answers in a few hundred
// ms against the ~1–3 s cold fork. Anything unavailable — no CRaC JDK, no
// image, an uncheckpointable jar, a restore hiccup — falls through to the
// cold path untouched, and a successful cold run seeds the checkpoint
// best-effort so the next command can restore.
func Call(ctx context.Context, jar, java string, req Request, env ...string) ([]byte, error) {
	req = withRequestEnv(req, env)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	kr := kernelrun.Req{
		Op:        req.Op,
		Workspace: req.Workspace,
		JVMFlags:  req.JVMFlags,
		Env:       env,
		Body:      body,
		Jar:       jar,
		Java:      java,
	}
	if out, code, err := kernelrun.Restore(ctx, kr); err == nil {
		if mapped := exitErr(code); mapped != nil {
			return out, &OpError{Op: req.Op, Err: mapped}
		}
		return out, nil
	}
	// JVM flags precede -jar: the kernel JVM runs on the workspace's pinned
	// java, and :rig/compile-jvm-opts must reach it (e.g. --enable-preview
	// for the AOT build).
	args := append(append([]string{}, req.JVMFlags...), "-jar", jar, "--request", "-")
	cmd := exec.CommandContext(ctx, java, args...)
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
		// Best-effort: run the same request through the kernel's checkpoint
		// bootstrap so a later command can restore. Silent on any failure;
		// the user's result is already in hand.
		_ = kernelrun.Warm(ctx, kr)
		return lastLine(out.Bytes()), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if mapped := exitErr(ee.ExitCode()); mapped != nil {
			return out.Bytes(), &OpError{Op: req.Op, Err: mapped}
		}
	}
	return out.Bytes(), err
}
