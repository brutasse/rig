package cli

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/classpath"
	"github.com/brutasse/rig/internal/fetch"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

// launchDefaults is rig's production JVM flag set, applied by `rig launch`
// before the module's :jvm-opts. The module opts come last, so for repeated
// flags the JVM applies the module's value; collectors are the exception —
// selecting two is a fatal VM error, so a collector in :jvm-opts replaces
// the G1 default (launchFlags). JMX is loopback-only by design (same-host
// monitoring) on a single fixed port.
var launchDefaults = []string{
	"-XX:+UseG1GC",
	"-XX:+AlwaysPreTouch",
	"-XX:+ExitOnOutOfMemoryError",
	"-XX:+HeapDumpOnOutOfMemoryError",
	"-Dcom.sun.management.jmxremote",
	"-Dcom.sun.management.jmxremote.port=10101",
	"-Dcom.sun.management.jmxremote.rmi.port=10101",
	"-Dcom.sun.management.jmxremote.authenticate=false",
	"-Dcom.sun.management.jmxremote.ssl=false",
	"-Djava.rmi.server.hostname=127.0.0.1",
}

// launchDescriptorPath is the entry the build bakes into every jar: the
// artifact's own launch plan.
const launchDescriptorPath = "META-INF/rig/launch.json"

// launchDescriptor is the launch plan baked into a rig-built jar: enough for
// `rig launch` to run the artifact without the workspace. Rig's production
// defaults are not baked in — the launching rig applies them, so the policy
// tracks rig upgrades even for old artifacts.
type launchDescriptor struct {
	Version int      `json:"version"`
	Rig     string   `json:"rig"`
	Main    string   `json:"main"`
	JVMOpts []string `json:"jvm-opts"`
	Java    int      `json:"java"` // feature version of the build JVM, 0 = unknown
	Uber    bool     `json:"uber"`
}

func newLaunchCmd(o *opts) *cobra.Command {
	c := &cobra.Command{
		Use:   "launch [jar] [args...]",
		Short: "Launch the built artifact with rig's production JVM flags",
		Long: `Launches the module's built jar or uberjar — rig's production entrypoint.

JVM flags, in order: rig's production defaults (G1 GC with AlwaysPreTouch,
exit on OOM, loopback-only JMX on port 10101), then the module's :jvm-opts,
which override the defaults (the last JVM flag wins; a :jvm-opts garbage
collector replaces the G1 default, since the JVM refuses two collectors),
then -jar or -cp and the main. The launch JVM's major version must exactly
match the one the artifact was built with.

The first positional is the jar to launch, when it names an existing file.
Otherwise, in a workspace, all positionals are passed to the main and the
jar is the target module's build output from the lock (the uberjar when one
is declared). With a jar, the launch plan (main, :jvm-opts, build JVM) comes
from the descriptor the build bakes into the jar — no workspace needed.

rig launch never re-locks and never hits the network: the lock is inert data
for it, a stale lock is ignored, and missing pieces (JDK, classpath
artifacts) are errors pointing at the command that prepares them.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLaunch(cmd.Context(), o, args)
		},
	}
	// Everything after the first positional arg is passed to the main.
	c.Flags().SetInterspersed(false)
	return c
}

// launchPlan is what rig launch runs: the main, the JVM flags that follow
// launchDefaults, whether the artifact is an uberjar, and the feature
// version of the JVM it was built with (0 = unknown).
type launchPlan struct {
	main    string
	jvmOpts []string
	uber    bool
	java    int
}

func runLaunch(ctx context.Context, o *opts, args []string) error {
	// rig launch is offline by definition: no JDK auto-install, no artifact
	// fetch, no passive update notice.
	o.offline = true

	// The workspace and lock are inert data for launch, never a gate: launch
	// runs the artifact it is pointed at — no staleness check, no re-lock.
	var (
		root *workspace.Root
		lock *lockfile.Document
	)
	if r, err := workspace.Find("."); err == nil {
		root, lock = r, r.Lock
	}

	// Positionals: the first one is the jar to launch, but only when it
	// names an existing file — otherwise (in a workspace) they are all main
	// arguments and the jar comes from the lock.
	var (
		jar      string
		explicit bool
		appArgs  = args
	)
	if len(args) > 0 {
		if st, err := os.Stat(args[0]); err == nil && !st.IsDir() {
			jar, explicit, appArgs = args[0], true, args[1:]
		}
	}

	// Resolve the jar (filesystem only, before any JVM work).
	m := ""
	if jar == "" {
		if lock == nil {
			if len(args) > 0 {
				return exitf(2, "no such jar: %s", args[0])
			}
			return exitf(2, "no jar given and no lock to find the build output (pass the jar)")
		}
		rm, rerr := resolveModule(root, o.path)
		if rerr != nil {
			return rerr
		}
		m = rm
		mod, err := lock.Module(m)
		if err != nil {
			return exitf(2, "module %s is not in the lock (run 'rig lock')", m)
		}
		out := firstBuildOutput(root, m, mod)
		if out == "" {
			return exitf(2, "module %s declares no build output", m)
		}
		jar = out
	}
	absJar, err := filepath.Abs(jar)
	if err != nil {
		return err
	}
	if st, err := os.Stat(absJar); err != nil || st.IsDir() {
		if explicit {
			return exitf(2, "no such jar: %s", jar)
		}
		return exitf(2, "no build output at %s — run 'rig build' (or 'rig build --uber')", jar)
	}

	store, err := o.store()
	if err != nil {
		return err
	}
	java, javaEnv, err := o.pickJava(ctx, store, root)
	if err != nil {
		return err
	}

	plan, err := launchPlanFor(o, lock, root, m, absJar)
	if err != nil {
		return err
	}
	if plan.main == "" {
		return exitf(2, "no main to launch (declare :rig/main and rebuild)")
	}
	if err := checkJavaVersion(java, plan.java); err != nil {
		return err
	}

	var runArgs []string
	if plan.uber {
		runArgs = launchFlags(plan.jvmOpts)
		runArgs = append(runArgs, "-jar", absJar)
	} else {
		if lock == nil {
			return exitf(2, "non-uber artifacts launch only in the workspace (their deps live in the lock)")
		}
		if m == "" {
			if o.path != "" {
				if m, err = resolveModule(root, o.path); err != nil {
					return err
				}
			} else {
				m = moduleOfOutput(lock, root, absJar)
			}
			if m == "" {
				return exitf(2, "cannot find the module owning %s (pass -p)", jar)
			}
		}
		cp, err := launchCP(ctx, o, root, lock, store, m, absJar)
		if err != nil {
			return err
		}
		runArgs = launchFlags(plan.jvmOpts)
		runArgs = append(runArgs, "-cp", cp, plan.main)
	}
	runArgs = append(runArgs, appArgs...)

	dir := ""
	if root != nil {
		dir = moduleDir(root, m)
	}
	return launch(jvm.Run{Java: java, Args: runArgs, Dir: dir, Env: javaEnv})
}

// gcFlagRe matches a garbage-collector selection flag (-XX:+UseZGC,
// -XX:-UseG1GC, …). Selecting two collectors at once is a fatal VM error,
// so when the module's :jvm-opts selects a collector, rig's G1 default is
// dropped rather than appended.
var gcFlagRe = regexp.MustCompile(`^-XX:[+-]Use[A-Za-z]*GC$`)

// launchFlags is the JVM flags of a launch: rig's production defaults, then
// the module's :jvm-opts (the JVM applies repeated flags last-wins, so the
// module opts override the defaults). The G1 default is dropped when
// :jvm-opts selects a collector.
func launchFlags(jvmOpts []string) []string {
	dropG1 := false
	for _, f := range jvmOpts {
		if gcFlagRe.MatchString(f) {
			dropG1 = true
			break
		}
	}
	var flags []string
	for _, f := range launchDefaults {
		if dropG1 && f == "-XX:+UseG1GC" {
			continue
		}
		flags = append(flags, f)
	}
	return append(flags, jvmOpts...)
}

// launchPlanFor resolves the launch plan of absJar: the descriptor the build
// baked into it, else the lock data of the module that owns it.
func launchPlanFor(o *opts, lock *lockfile.Document, root *workspace.Root, m, absJar string) (launchPlan, error) {
	desc, err := readLaunchDescriptor(absJar)
	if err != nil {
		if errors.Is(err, errNotZip) {
			return launchPlan{}, exitf(2, "%s: not a readable jar", absJar)
		}
		return launchPlan{}, exitf(1, "%s: %v", absJar, err)
	}
	if desc != nil {
		return launchPlan{main: desc.Main, jvmOpts: desc.JVMOpts, uber: desc.Uber, java: desc.Java}, nil
	}
	// No descriptor: a legacy artifact (or one not built by rig). The lock is
	// the plan — which requires a module.
	if lock == nil {
		return launchPlan{}, exitf(2, "%s: not a rig-built artifact (no %s) — cd into the project, or build with rig", absJar, launchDescriptorPath)
	}
	if m == "" {
		if o.path != "" {
			if m, err = resolveModule(root, o.path); err != nil {
				return launchPlan{}, err
			}
		} else {
			m = moduleOfOutput(lock, root, absJar)
		}
	}
	if m == "" {
		return launchPlan{}, exitf(2, "%s: not a rig-built artifact (no %s) and no module in the lock owns it", absJar, launchDescriptorPath)
	}
	mod, err := lock.Module(m)
	if err != nil {
		return launchPlan{}, exitf(2, "module %s is not in the lock (run 'rig lock')", m)
	}
	isUber := mod.Build.Uberjar != nil && absJar == filepath.Join(root.Dir, m, mod.Build.Uberjar.File)
	main := mod.Main
	if isUber && mod.Build.Uberjar.Main != "" {
		main = mod.Build.Uberjar.Main
	}
	java := 0
	if lock.JVM != nil {
		v := lock.JVM.Version
		if v == "" {
			v = lock.JVM.Requested
		}
		java = featureVersion(v)
	}
	return launchPlan{main: main, jvmOpts: mod.JVMOpts, uber: isUber, java: java}, nil
}

var errNotZip = errors.New("not a jar")

// readLaunchDescriptor returns the descriptor baked into jar, or nil when the
// entry is absent.
func readLaunchDescriptor(jar string) (*launchDescriptor, error) {
	zf, err := zip.OpenReader(jar)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotZip, err)
	}
	defer zf.Close()
	for _, f := range zf.File {
		if f.Name != launchDescriptorPath {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		var d launchDescriptor
		if err := json.NewDecoder(r).Decode(&d); err != nil {
			return nil, fmt.Errorf("bad %s: %v", launchDescriptorPath, err)
		}
		return &d, nil
	}
	return nil, nil
}

// buildOutputs are the module's build output paths (absolute), mirroring
// buildOne.
func buildOutputs(root *workspace.Root, m string, mod lockfile.Module) []string {
	var out []string
	if mod.Build.Uberjar != nil {
		out = append(out, filepath.Join(root.Dir, m, mod.Build.Uberjar.File))
	}
	if mod.Build.Jar {
		out = append(out, filepath.Join(root.Dir, m, "target", jarFileName(mod)))
	}
	return out
}

// firstBuildOutput is the artifact `rig launch` targets for the module: the
// uberjar when one is declared, else the jar.
func firstBuildOutput(root *workspace.Root, m string, mod lockfile.Module) string {
	if out := buildOutputs(root, m, mod); len(out) > 0 {
		return out[0]
	}
	return ""
}

// moduleOfOutput is the lock module that owns absJar as a build output, ""
// when none (or more than one) does.
func moduleOfOutput(lock *lockfile.Document, root *workspace.Root, absJar string) string {
	var found string
	for m, mod := range lock.Modules {
		for _, p := range buildOutputs(root, m, mod) {
			if p == absJar {
				if found != "" && found != m {
					return ""
				}
				found = m
			}
		}
	}
	return found
}

// checkJavaVersion enforces an exact match on the JVM major version: the
// launch JVM must be the same feature version as the one the artifact was
// built with (patch versions are irrelevant; a different major is refused in
// both directions — the artifact runs on the JVM it was built with). want is
// 0 when the build JVM is unknown.
func checkJavaVersion(java string, want int) error {
	if want == 0 {
		return nil
	}
	v, err := jvm.Version(java)
	if err != nil {
		return exitf(1, "cannot determine the java version: %v", err)
	}
	got := featureVersion(v)
	if !javaVersionOK(got, want) {
		if got == 0 {
			return exitf(2, "cannot determine the java feature version from %s", v)
		}
		return exitf(2, "artifact built with Java %d; current java is %s — exact match required: install Java %d ('rig jvm install %d') or set JAVA_HOME", want, v, want, want)
	}
	return nil
}

// javaVersionOK is the exact-match decision: the launch JVM's feature
// version must equal the build JVM's. want==0 = nothing to check, always
// ok; got==0 = launch version undetermined (fails when there is something
// to check).
func javaVersionOK(got, want int) bool {
	return want == 0 || got == want
}

// featureVersion is the JVM feature version of a version string:
// "21.0.12" → 21, "1.8.0_422" → 8. 0 when unparseable.
func featureVersion(v string) int {
	parts := strings.SplitN(v, ".", 3)
	if parts[0] == "1" && len(parts) > 1 {
		parts = parts[1:]
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	return n
}

// launchCP assembles the non-uber launch classpath: the module's locked
// classpath with its own source paths replaced by the built jar.
func launchCP(ctx context.Context, o *opts, root *workspace.Root, lock *lockfile.Document, store *cache.Store, m, absJar string) (string, error) {
	gitlibs, err := classpath.GitlibsRoot()
	if err != nil {
		return "", err
	}
	e := &hotEnv{root: root, lock: lock, store: store, client: fetch.New(true), m2root: o.m2Root(), gitlibs: gitlibs}
	entries, err := e.entriesOf(ctx, m, "")
	if err != nil {
		return "", err
	}
	swapped := false
	for i := range entries {
		if entries[i].ID == "paths:"+m {
			entries[i].Paths = []string{absJar}
			swapped = true
		}
	}
	if !swapped {
		entries = append([]classpath.Entry{{ID: "built-jar", Paths: []string{absJar}}}, entries...)
	}
	return strings.Join(classpath.Flatten(entries), string(filepath.ListSeparator)), nil
}
