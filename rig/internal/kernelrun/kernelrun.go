// Package kernelrun runs restore-eligible kernel ops on a CRaC checkpoint
// of a warmed kernel instead of forking a cold JVM, and refreshes the
// checkpoint with every run so the image self-warms with use.
//
// Everything here is opportunistic. A missing capability (non-Linux, no
// CRaC-capable JDK, a kernel jar without checkpoint entries), a missing or
// capped image chain, or a restore that misbehaves in any way degrades to
// the plain cold kernel.Call path — the cold path remains the correctness
// anchor.
//
// The checkpoint JVM is RIG_CRAC_JDK when set, else the managed Zulu CRaC
// build once 'rig crac install' has installed it (see jdk.CRaCPins), else
// the workspace's cold JVM — in every case gated by the engine probe.
//
// The JVM contract encoded below comes from the P0 spike
// (.scratch/crac-spike/RESULTS.md); breaking one of these rules corrupts
// images or crashes restores:
//
//   - direct_map=false on every restore: the default maps the restored
//     process's memory shared from core.img, so the process rewrites its
//     own image file.
//   - An explicit CRaCCheckpointTo on every restore: the dump target is
//     baked into the image, and a restore without an override re-dumps
//     over its own source mid-restore.
//   - The kernel parks inside checkpointRestore() called from its original
//     main; the restore-side new main (rig.kernel.Runner) runs the op and
//     RETURNS into that loop, which re-dumps the refreshed image before
//     the engine kills the process after the dump.
//   - Success of a refresh run = post-dump kill + result JSON on stdout +
//     core.img present in the target dir: a failed dump can still exit 0,
//     so the image on disk, not the exit code, decides. CRaC banner lines
//     pollute stdout and stderr alike (ignored-fd policy), hence the
//     stdio noise filter.
//   - Refresh dumps are deltas referencing their parent images, so the
//     whole img-NNN chain must stay on disk until a fresh bootstrap
//     replaces it; the chain cap bounds the disk cost.
//
// Credentials ride the REQUEST FILE, not the environment: System/getenv is
// boot-cached inside a JVM, so a restored kernel would otherwise read the
// bootstrap run's tokens forever — and a tokenless workspace restoring an
// image that an authenticated one bootstrapped would leak those tokens out
// of the heap. The kernel treats the request's :env map as authoritative
// (rig.resolver.main/run-request-file), and kernel.Call populates it
// (kernel.Request.Env). What remains true: dumps capture the heap, so an
// image built from an authenticated run may contain token residue until GC
// clears it — the whole store is owner-only (0700 dirs, 0600 temp request
// files deleted after use) and holds only the user's own credentials.
package kernelrun

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
)

// ErrUnavailable means this call cannot use the checkpoint path (no
// capability, no image, restore failed at the infrastructure level). The
// caller must run the op cold; nothing is promoted or deleted.
var ErrUnavailable = errors.New("kernelrun: checkpoint path unavailable")

const (
	// bootstrapMain and runnerMain are AOT'd entry classes inside the
	// kernel jar (P2). Released jars (v0.2 and older) lack them and stay
	// fully cold, see jarCheckpointable.
	bootstrapMain = "rig.kernel.Bootstrap"
	runnerMain    = "rig.kernel.Runner"

	// chainCap bounds img-NNN accumulation: refresh dumps are deltas that
	// reference ALL their ancestors, so old chain members are only
	// deletable as a whole (rig clean / re-bootstrap, P4).
	chainCap = 6

	// bootstrapXmx sizes the checkpoint heap; the restored JVM runs with
	// whatever the dump recorded, so this one number sizes every image.
	bootstrapXmx = "-Xmx512m"
)

// restoreOps are read-only or rerun-idempotent, and — with the JVMFlags
// gate on top — never depend on state that differs between checkpoint and
// restore time.
var restoreOps = map[string]bool{
	"resolve": true, "check": true, "tree": true, "outdated": true,
	"aot-plan": true, "edit-dep": true, "migrate": true,
}

// cracBanner matches the CRaC engine's stdout/stderr banner lines
// ("[1234.567s][info][crac] ..."), which the engine writes to the
// inherited fds AFTER the op printed its result.
var cracBanner = regexp.MustCompile(`^\[[0-9.]+s\]\[`)

// warpLog matches the warp engine's timestamped status lines
// ("7243.697745: warp: Checkpoint ...", "7243.749486: Restore open file
// descriptors"), which accompany every dump on stderr.
var warpLog = regexp.MustCompile(`^\d+\.\d{6}: `)

// noise reports a stderr/stdout line emitted by the checkpoint machinery
// rather than by the kernel op.
func noise(line string) bool {
	return cracBanner.MatchString(line) || warpLog.MatchString(line) ||
		strings.HasPrefix(line, "INFO: ") || strings.Contains(line, "jdk.internal.crac.")
}

// Req describes one kernel op as kernel.Call would run it cold.
type Req struct {
	Op        string
	Workspace string   // anchors cwd for bootstrap AND restore: restored processes keep the checkpointed cwd, so both phases pin it to the workspace, never to whatever dir a command ran from
	JVMFlags  []string // restore can only serve flag-free ops: JVM flags are baked at checkpoint time
	Env       []string // extra env for the checkpoint JVMs (subprocess inheritance; request-config keys reach the kernel via Body's :env map instead, see kernel.Request.Env)
	Body      []byte   // request JSON
	Jar       string
	Java      string // the JVM rig picked for the cold path
}

func eligible(r Req) bool {
	if runtime.GOOS != "linux" || !restoreOps[r.Op] || len(r.JVMFlags) > 0 {
		return false
	}
	return r.Workspace != "" && len(r.Body) > 0 && r.Jar != ""
}

// root is the checkpoint store, override-able for tests and dev via
// RIG_CRAC_DIR; default <rig data dir>/crac.
func root() (string, error) {
	if d := os.Getenv("RIG_CRAC_DIR"); d != "" {
		return d, nil
	}
	st, err := cache.New()
	if err != nil {
		return "", err
	}
	return filepath.Join(st.Root, "crac"), nil
}

// cracJVMs lists the checkpoint JVMs in selection order: an explicit
// RIG_CRAC_JDK selects ONLY it (it declares which JVM to use); otherwise
// the managed install (once 'rig crac install' put it in the store)
// precedes the cold-path JVM. Unprobed: callers gate with probeJDK.
func cracJVMs(coldJava string) []string {
	if p := os.Getenv("RIG_CRAC_JDK"); p != "" {
		return []string{p}
	}
	var out []string
	if st, err := cache.New(); err == nil {
		if inst, err := jdk.CRaCInstalled(jdk.NewStoreAt(st.Root)); err == nil {
			out = append(out, inst.JavaPath)
		}
	}
	if coldJava != "" {
		out = append(out, coldJava)
	}
	return out
}

// jdkFor picks the JVM that runs the checkpoint phases (bootstrap and
// restore MUST share one build — the image key pins it): the first
// cracJVMs candidate, falling back to the cold-path JVM when there is
// none (the probe then parks the whole feature).
func jdkFor(r Req) string {
	if cs := cracJVMs(r.Java); len(cs) > 0 {
		return cs[0]
	}
	return r.Java
}

var (
	probeMu     sync.Mutex
	probeResult = map[string][2]string{} // java path -> {buildID, "ok"}; zero entry = cached probe failure
)

// probeJDK checks (once per process per path) that a JVM carries the CRaC
// engine flags and returns a stable build identity for the image key. One
// fork prints both the version line and the flag table.
func probeJDK(java string) (string, bool) {
	probeMu.Lock()
	defer probeMu.Unlock()
	if v, seen := probeResult[java]; seen {
		return v[0], v[1] == "ok"
	}
	ok := false
	build := ""
	out, err := exec.Command(java,
		"-XX:+UnlockDiagnosticVMOptions", "-XX:+PrintFlagsFinal", "-version").CombinedOutput()
	if err == nil {
		s := string(out)
		if strings.Contains(s, "CRaCCheckpointTo") {
			ok = true
			build = strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
		}
	}
	id := ""
	if ok {
		sum := sha256.Sum256([]byte(strings.TrimSpace(build)))
		id = hex.EncodeToString(sum[:8])
	}
	if ok {
		probeResult[java] = [2]string{id, "ok"}
	} else {
		probeResult[java] = [2]string{}
	}
	return id, ok
}

var (
	machineOnce sync.Once
	machineID   string
)

// machineFingerprint hashes the CPU feature flags: warp refuses restores
// on foreign CPUs, but failing late (crash → cold fallback) is messier
// than keying the image on the features it will demand.
func machineFingerprint() string {
	machineOnce.Do(func() {
		id := "cpu-unknown"
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			var feat []string
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "flags") || strings.HasPrefix(line, "Features") {
					feat = append(feat, line)
				}
			}
			if len(feat) > 0 {
				sum := sha256.Sum256([]byte(strings.Join(feat, "\n")))
				id = hex.EncodeToString(sum[:16])
			}
		}
		machineID = id
	})
	return machineID
}

var (
	jarMu     sync.Mutex
	jarProbed = map[string]bool{}
)

// jarCheckpointable caches whether the kernel jar carries the checkpoint
// entry classes. Released jars (v0.2 and older) don't — this is the cheap
// gate that keeps cold-path overhead at zero for them.
func jarCheckpointable(jar string) bool {
	jarMu.Lock()
	defer jarMu.Unlock()
	if v, seen := jarProbed[jar]; seen {
		return v
	}
	ok := false
	if z, err := zip.OpenReader(jar); err == nil {
		defer z.Close()
		want := map[string]bool{
			strings.ReplaceAll(bootstrapMain, ".", "/") + ".class": true,
			strings.ReplaceAll(runnerMain, ".", "/") + ".class":    true,
		}
		found := 0
		for _, f := range z.File {
			if want[f.Name] {
				found++
				delete(want, f.Name)
			}
		}
		ok = found == 2
	}
	jarProbed[jar] = ok
	return ok
}

// keyFor binds an image directory to everything a checkpoint silently
// depends on: the exact kernel bytes, the exact JVM build, the engine, the
// CPU features. Any change re-keys to an empty dir = cold until re-warmed.
func keyFor(jar, buildID string) (string, error) {
	jarSum, err := digest.File(jar)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{jarSum, buildID, "warp", machineFingerprint()}, "\n")))
	return hex.EncodeToString(sum[:16]), nil
}

func images(keyDir string) []string {
	ents, err := os.ReadDir(keyDir)
	if err != nil {
		return nil
	}
	var imgs []string
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "img-") {
			imgs = append(imgs, e.Name())
		}
	}
	sort.Strings(imgs) // img-NNN, zero-padded: lexical == numeric
	return imgs
}

// Restore runs the request on the newest checkpoint image and refreshes
// the image with the state this op just warmed. It returns the kernel's
// result JSON (single line, banners filtered) and the op exit code mapped
// exactly like the cold path's (success 0, op failures 1/2/3). Every
// infrastructure problem — including a dump that failed after the op
// printed — returns an error so the caller re-runs the op cold; eligible
// ops are idempotent, so a re-run is safe.
func Restore(ctx context.Context, r Req) ([]byte, int, error) {
	if !eligible(r) {
		return nil, 0, ErrUnavailable
	}
	if !jarCheckpointable(r.Jar) {
		return nil, 0, ErrUnavailable
	}
	java := jdkFor(r)
	buildID, ok := probeJDK(java)
	if !ok {
		return nil, 0, ErrUnavailable
	}
	kd, err := keyDir(r, buildID)
	if err != nil {
		return nil, 0, err
	}
	imgs := images(kd)
	if len(imgs) == 0 || len(imgs) >= chainCap {
		return nil, 0, ErrUnavailable // none yet / chain capped (P4 rebuilds)
	}
	reqFile, err := writeReq(kd, r.Body)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(reqFile)
	next := filepath.Join(kd, fmt.Sprintf("next-%d", os.Getpid()))
	defer os.RemoveAll(next)

	// Every restore flag here is load-bearing; see the package doc.
	args := []string{
		"-XX:CRaCRestoreFrom=" + filepath.Join(kd, imgs[len(imgs)-1]),
		"-XX:CRaCCheckpointTo=" + next,
		"-XX:CRaCEngineOptions=direct_map=false,log_level=warn",
		"-XX:+UnlockDiagnosticVMOptions",
		"-XX:+IgnoreUnrecognizedVMOptions",
		"-XX:CRaCIgnoredFileDescriptors=0,1,2",
		runnerMain, reqFile,
	}
	out, code, err := runKernelJVM(ctx, java, args, r, newNoiseFilter(os.Stderr))
	if err != nil {
		return nil, 0, err
	}
	if dumped(next) {
		// 137 is the engine's post-dump SIGKILL — the contract's success
		// signal. A dump plus an unexpected code means a JVM we don't
		// recognize the behavior of: treat the op result as unreliable.
		if code != 137 && code != 0 {
			return nil, 0, ErrUnavailable
		}
		promote(next, filepath.Join(kd, fmt.Sprintf("img-%03d", len(imgs))))
		return resultLine(out), 0, nil
	}
	switch code {
	case 1, 2, 3:
		// The op failed on the warm heap exactly like it would cold: the
		// result stands, the image deliberately does NOT refresh (the
		// Runner exits before returning into the parked loop).
		return filtered(out), code, nil
	case 0:
		// Ran clean but the loop's dump did not land (engine config edge):
		// result valid, image stays as-is.
		return resultLine(out), 0, nil
	default:
		return nil, 0, ErrUnavailable
	}
}

// Warm builds a fresh checkpoint by running the real request through the
// kernel's bootstrap entry: warm the kernel, run the op once for real,
// park and dump. Call after a successful cold op of an eligible request;
// it is best-effort by contract — failures are silent, the cold path
// already produced the user's result. The request runs AGAIN, which the
// restoreOps idempotence rule makes safe; its :env map (kernel.Request.Env)
// carries the same fresh config the cold run saw.
func Warm(ctx context.Context, r Req) error {
	if !eligible(r) || !jarCheckpointable(r.Jar) {
		return ErrUnavailable
	}
	java := jdkFor(r)
	buildID, ok := probeJDK(java)
	if !ok {
		return ErrUnavailable
	}
	kd, err := keyDir(r, buildID)
	if err != nil {
		return err
	}
	if len(images(kd)) >= chainCap {
		return ErrUnavailable // disk bound; P4's clean/rebuild resets it
	}
	if err := os.MkdirAll(kd, 0o700); err != nil {
		return err
	}
	reqFile, err := writeReq(kd, r.Body)
	if err != nil {
		return err
	}
	defer os.Remove(reqFile)
	warm := filepath.Join(kd, fmt.Sprintf("warm-%d", os.Getpid()))
	defer os.RemoveAll(warm)

	args := []string{
		"-XX:CRaCCheckpointTo=" + warm,
		"-XX:+UnlockDiagnosticVMOptions",
		"-XX:+IgnoreUnrecognizedVMOptions",
		"-XX:CRaCIgnoredFileDescriptors=0,1,2",
		bootstrapXmx,
		"-cp", r.Jar, // -jar would pin the cold Main-Class
		bootstrapMain, reqFile,
	}
	// Bootstrap failure chatter is not the user's business: this op
	// already ran successfully cold.
	_, code, err := runKernelJVM(ctx, java, args, r, io.Discard)
	if err != nil || code != 137 || !dumped(warm) {
		return ErrUnavailable
	}
	promote(warm, filepath.Join(kd, fmt.Sprintf("img-%03d", len(images(kd)))))
	writeChainMeta(kd, r.Jar, buildID)
	return nil
}

// chainMeta records, inside the chain dir, which kernel jar the chain was
// bootstrapped from. Chain keys are opaque hashes, so without this a jar
// rotation is only decidable through probe-based key matching; the meta
// makes "chain of an old kernel" answerable from a directory listing —
// which is what lets the boot sweep run with no probe and no fork.
type chainMeta struct {
	Jar string `json:"jar"` // sha256 of the kernel jar bytes
	JDK string `json:"jdk"` // probeJDK build identity
}

const chainMetaFile = "chain.json"

func writeChainMeta(dir, jar, buildID string) {
	sum, err := digest.File(jar)
	if err != nil {
		return
	}
	if b, err := json.Marshal(chainMeta{Jar: sum, JDK: buildID}); err == nil {
		_ = os.WriteFile(filepath.Join(dir, chainMetaFile), b, 0o600)
	}
}

func readChainMeta(dir string) chainMeta {
	var m chainMeta
	b, _ := os.ReadFile(filepath.Join(dir, chainMetaFile))
	_ = json.Unmarshal(b, &m)
	return m
}

// sweepCadence bounds how often the boot sweep inspects the store.
const sweepCadence = 24 * time.Hour

// SweepDue reports whether the daily boot sweep should run: a checkpoint
// store exists and the marker inside it is missing or stale. A machine
// that never checkpointed answers false on a single stat and never
// creates the store.
func SweepDue() bool {
	base, err := root()
	if err != nil {
		return false
	}
	if _, err := os.Stat(base); err != nil {
		return false
	}
	m, err := os.Stat(filepath.Join(base, ".swept"))
	if err != nil {
		return true
	}
	return time.Since(m.ModTime()) >= sweepCadence
}

// SweepDaily prunes chains whose chainMeta names a kernel jar other than
// jar — chains a kernel rotation has orphaned. No probe: chains without
// meta, and same-jar chains under any JVM, are left for the full
// 'rig crac clean'. Stamps the marker on success.
func SweepDaily(jar string) (int, int64, error) {
	base, err := root()
	if err != nil {
		return 0, 0, err
	}
	n, freed, err := sweepStaleJar(jar)
	if err != nil {
		return n, freed, err
	}
	_ = os.WriteFile(filepath.Join(base, ".swept"), nil, 0o600)
	return n, freed, nil
}

func sweepStaleJar(jar string) (int, int64, error) {
	base, err := root()
	if err != nil {
		return 0, 0, err
	}
	jarSum, err := digest.File(jar)
	if err != nil {
		return 0, 0, err
	}
	ents, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	n, freed := 0, int64(0)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		if m := readChainMeta(dir); m.Jar != "" && m.Jar != jarSum {
			f, err := dirSize(dir)
			if err != nil {
				return n, freed, err
			}
			if err := os.RemoveAll(dir); err != nil {
				return n, freed, err
			}
			n++
			freed += f
		}
	}
	return n, freed, nil
}

func keyDir(r Req, buildID string) (string, error) {
	base, err := root()
	if err != nil {
		return "", err
	}
	key, err := keyFor(r.Jar, buildID)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, key), nil
}

// Clean prunes checkpoint chains that can never restore on this machine
// again. The key pins a chain to the exact kernel bytes, the exact JVM
// build, the engine and the CPU flags: rotate the kernel jar or the JDK
// and the chain is orphaned. Every chain whose candidate JVM (cracJVMs
// over the system java — the machine default; a workspace-pinned JVM
// clean cannot see) still passes the probe is kept, everything else is
// pruned along with its whole img-NNN chain — refresh deltas reference
// their ancestors, so only the set is deletable. Chains are regenerable
// data: pruning one that was still live costs a cold run and a
// re-bootstrap, nothing more. Interrupted refresh leftovers (next-*) and
// orphaned request files (req-*.json) are swept from kept chains too.
// Returns the number of pruned chains and the bytes freed.
func Clean(jar string) (int, int64, error) {
	base, err := root()
	if err != nil {
		return 0, 0, err
	}
	cold := ""
	if p, err := jvm.Find(); err == nil {
		cold = p
	}
	valid := map[string]bool{}
	for _, java := range cracJVMs(cold) {
		if buildID, ok := probeJDK(java); ok {
			if k, err := keyFor(jar, buildID); err == nil {
				valid[k] = true
			}
		}
	}
	ents, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	chains, freed := 0, int64(0)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		if valid[e.Name()] {
			n, f := sweepTransient(dir)
			chains += n
			freed += f
			continue
		}
		f, err := dirSize(dir)
		if err != nil {
			return chains, freed, err
		}
		if err := os.RemoveAll(dir); err != nil {
			return chains, freed, err
		}
		chains++
		freed += f
	}
	return chains, freed, nil
}

// sweepTransient removes crash leftovers from a kept chain dir: next-*
// staging dumps and req-*.json request files (both are always deleted on
// a clean run). Returns what it counted, reported like a pruned chain.
func sweepTransient(dir string) (int, int64) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	n, freed := 0, int64(0)
	for _, e := range ents {
		name := e.Name()
		transient := strings.HasPrefix(name, "next-") ||
			(!e.IsDir() && strings.HasPrefix(name, "req-") && strings.HasSuffix(name, ".json"))
		if !transient {
			continue
		}
		p := filepath.Join(dir, name)
		var f int64
		if e.IsDir() {
			f, _ = dirSize(p)
		} else if st, err := e.Info(); err == nil {
			f = st.Size()
		}
		if err := os.RemoveAll(p); err != nil {
			continue
		}
		n++
		freed += f
	}
	return n, freed
}

func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func writeReq(dir string, body []byte) (string, error) {
	f, err := os.CreateTemp(dir, "req-*.json")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// runKernelJVM forks a checkpoint-phase JVM and normalizes its exit: the
// engine's post-dump SIGKILL arrives as code 137 whether the process was
// killed by signal (Go reports -1) or the engine suppressed the signal for
// an exit code; any other signal death reports -1.
func runKernelJVM(ctx context.Context, java string, args []string, r Req, stderr io.Writer) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, java, args...)
	cmd.Dir = r.Workspace
	cmd.Env = append(os.Environ(), "CLOJURE_CLI_ALLOW_HTTP_REPO=1")
	cmd.Env = append(cmd.Env, r.Env...)
	cmd.Stderr = stderr
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return nil, 0, err // java didn't start: infrastructure failure
	}
	code := ee.ExitCode()
	if code == -1 {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() &&
			ws.Signal() == syscall.SIGKILL {
			code = 137
		}
	}
	return out.Bytes(), code, nil
}

func dumped(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "core.img"))
	return err == nil && !st.IsDir() && st.Size() > 0
}

// promote lands a freshly dumped image as the chain's newest. A losing
// rename race is a no-op win: the other process's image is just as warm.
func promote(dir, target string) {
	if os.MkdirAll(filepath.Dir(target), 0o700) != nil {
		return
	}
	if os.Rename(dir, target) == nil {
		os.Chmod(target, 0o700)
	}
}

// resultLine mirrors the cold path's lastLine contract on filtered output.
func resultLine(b []byte) []byte {
	f := bytes.TrimRight(filtered(b), "\n")
	if i := bytes.LastIndexByte(f, '\n'); i >= 0 {
		return f[i+1:]
	}
	return f
}

func filtered(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	keep := lines[:0]
	for _, l := range lines {
		if !noise(l) {
			keep = append(keep, l)
		}
	}
	return []byte(strings.Join(keep, "\n"))
}

// noiseFilter streams stderr, dropping CRaC/JUL checkpoint chatter and
// passing everything else through line-wise.
type noiseFilter struct {
	w     io.Writer
	carry []byte
}

func newNoiseFilter(w io.Writer) io.Writer { return &noiseFilter{w: w} }

func (n *noiseFilter) Write(p []byte) (int, error) {
	n.carry = append(n.carry, p...)
	for {
		i := bytes.IndexByte(n.carry, '\n')
		if i < 0 {
			break
		}
		line := string(n.carry[:i])
		if !noise(line) {
			if _, err := fmt.Fprintln(n.w, line); err != nil {
				return 0, err
			}
		}
		n.carry = n.carry[i+1:]
	}
	return len(p), nil
}
