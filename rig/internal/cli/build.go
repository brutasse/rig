package cli

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/classpath"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
)

func newBuildCmd(o *opts) *cobra.Command {
	var uber bool
	c := &cobra.Command{
		Use:   "build",
		Short: "Build the target module's jar or uberjar",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd.Context(), o, uber)
		},
	}
	c.Flags().BoolVar(&uber, "uber", false, "build the uberjar instead of the jar")
	return c
}

func runBuild(ctx context.Context, o *opts, uber bool) error {
	e, err := o.hot(ctx, true)
	if err != nil {
		return err
	}
	m, err := e.targetModule(o.path)
	if err != nil {
		return err
	}
	out, err := e.buildOne(ctx, m, uber)
	if err != nil {
		return err
	}
	fmt.Printf("built %s\n", out)
	return nil
}

// buildOne builds module m's jar (or the uberjar when uber) and returns the
// output path. Shared by build, install and publish.
func (e *hotEnv) buildOne(ctx context.Context, m string, uber bool) (string, error) {
	mod, err := e.module(m)
	if err != nil {
		return "", err
	}

	buildJar := mod.Build.Jar && !uber
	buildUber := uber && mod.Build.Uberjar != nil
	if !buildJar && !buildUber {
		if uber {
			return "", exitf(2, "module %s declares no uberjar", m)
		}
		return "", exitf(2, "module %s declares no jar", m)
	}

	if err := e.ensurePreps(ctx, m, mod); err != nil {
		return "", err
	}

	entries, err := e.entriesOf(ctx, m, "")
	if err != nil {
		return "", err
	}
	dir := e.modDir(m)

	// tools.build's uber only explodes libs that are directories or end in
	// ".jar"; the cache stores artifacts content-addressably (no extension),
	// so stage mvn jars under their canonical name for the build.
	arts := make(map[string]lockfile.Artifact, len(e.lock.Artifacts))
	for _, a := range e.lock.Artifacts {
		arts[a.ID] = a
	}
	entries, cleanup, err := stageJars(entries, arts)
	if err != nil {
		return "", err
	}
	defer cleanup()

	cps := make([]map[string]any, 0, len(entries))
	for _, en := range entries {
		cps = append(cps, map[string]any{"id": en.ID, "paths": en.Paths})
	}

	cfg := map[string]any{
		"dir":           dir,
		"classpath":     cps,
		"src-dirs":      absJoin(dir, mod.Build.SrcDirs),
		"java-src-dirs": absJoin(dir, mod.Build.JavaSrcDirs),
		"class-dir":     filepath.Join(dir, mod.Build.ClassDir),
		"jar?":          buildJar,
		"jar-file":      filepath.Join(dir, "target", jarFileName(mod)),
		"uber?":         buildUber,
	}
	// An empty main is the absence of a main: passing it through would make
	// tools.build write an empty Main-Class manifest attribute.
	if mod.Main != "" {
		cfg["main"] = mod.Main
	}
	if len(mod.Build.NsCompile) > 0 {
		cfg["ns-compile"] = mod.Build.NsCompile
	}
	if opts := javacOptsOf(e.lock.JVM, mod.Build.JavacOpts); len(opts) > 0 {
		cfg["javac-opts"] = opts
	}
	if buildUber {
		cfg["uber-file"] = filepath.Join(dir, mod.Build.Uberjar.File)
		if mod.Build.Uberjar.Main != "" {
			cfg["main"] = mod.Build.Uberjar.Main
		}
		if ex, ok := mod.Build.Uberjar.Opts["exclude"]; ok {
			cfg["exclude"] = ex
		}
		warnDuplicateEntries(entries)
	}
	// Bake the launch plan into the artifact (META-INF/rig/launch.json) so
	// `rig launch <jar>` can run it without the workspace. Rig's production
	// defaults are not baked in — the launching rig applies them; only the
	// module's own :jvm-opts travel with the artifact.
	if main, _ := cfg["main"].(string); main != "" {
		launch := map[string]any{
			"version":  1,
			"rig":      Version,
			"main":     main,
			"jvm-opts": mod.JVMOpts,
			"uber":     buildUber,
		}
		if v, err := jvm.Version(e.java); err == nil {
			launch["java"] = jdk.FeatureVersion(v)
		}
		cfg["launch"] = launch
	}

	resp, err := kernel.Call(ctx, e.kernel, e.java, kernel.Request{
		Op:        "build",
		Workspace: e.root.Dir,
		Modules:   []string{m},
		Args:      map[string]any{"builds": map[string]any{m: cfg}},
	})
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return "", exitf(1, "build: %v", oe)
		}
		return "", err
	}

	var parsed struct {
		Results []struct {
			Module   string `json:"module"`
			ClassDir string `json:"class-dir"`
			Jar      string `json:"jar"`
			Uber     string `json:"uber"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", fmt.Errorf("build: bad kernel response: %w", err)
	}
	if len(parsed.Results) == 0 {
		return "", fmt.Errorf("build: kernel returned no results")
	}
	r := parsed.Results[0]
	if r.Jar != "" {
		return r.Jar, nil
	}
	return r.Uber, nil
}

// stageJars copies the mvn jar artifacts of entries into a temp dir under a
// flat, colon-free canonical name (group-name-version). Non-jar entries
// (local paths, git checkouts) pass through. Returns the restaged entries
// and a cleanup func.
func stageJars(entries []classpath.Entry, arts map[string]lockfile.Artifact) ([]classpath.Entry, func(), error) {
	tmp, err := os.MkdirTemp("", "rig-build-libs-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	out := make([]classpath.Entry, 0, len(entries))
	for _, en := range entries {
		a, ok := arts[en.ID]
		if !ok || a.Kind != "mvn" || a.Extension != "jar" || len(en.Paths) != 1 {
			out = append(out, en)
			continue
		}
		// Flat, colon-free name (the classpath separator is ":" on Unix, so a
		// path containing ":" would be split by the JVM): group-name-version.
		name := a.Group + "-" + a.Name + "-" + a.Version
		if a.Classifier != nil && *a.Classifier != "" {
			name += "-" + *a.Classifier
		}
		name += "." + a.Extension
		dst := filepath.Join(tmp, name)
		if err := copyFile(en.Paths[0], dst); err != nil {
			cleanup()
			return nil, nil, err
		}
		out = append(out, classpath.Entry{ID: en.ID, Paths: []string{dst}})
	}
	return out, cleanup, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func absJoin(dir string, rels []string) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = filepath.Join(dir, r)
	}
	return out
}

// uberExcluded mirrors tools.build's default uber exclusions: entries the
// merge drops anyway, so duplicates of them can never shadow anything.
var uberExcluded = []*regexp.Regexp{
	regexp.MustCompile(`^project\.clj$`),
	regexp.MustCompile(`^META-INF/.*\.(?:SF|RSA|DSA|MF)$`),
	regexp.MustCompile(`module-info\.class`),
	regexp.MustCompile(`(?:^|.*/)\.DS_Store$`),
	regexp.MustCompile(`(?:^|.*/)\.keep$`),
	regexp.MustCompile(`.*\.pom$`),
	regexp.MustCompile(`(?i)^META-INF/(?:INDEX\.LIST|DEPENDENCIES)(?:\.txt)?$`),
}

func excludedFromUber(name string) bool {
	for _, re := range uberExcluded {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// entrySHA256 hashes the entry of a jar, "" when it cannot be read.
func entrySHA256(zf *zip.ReadCloser, name string) string {
	r, err := zf.Open(name)
	if err != nil {
		return ""
	}
	defer r.Close()
	h := sha256.New()
	_, _ = io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil))
}

// warnDuplicateEntries reports jar entries present in more than one classpath
// jar with differing content, grouped by package: the uber keeps the first in
// classpath order, which may be a stale vendored copy shadowing the dependency
// it duplicates. Identical content (and tools.build's excluded entries) is a
// no-op and stays silent.
func warnDuplicateEntries(entries []classpath.Entry) {
	type occ struct {
		id, file, name string
	}
	zfs := make(map[string]*zip.ReadCloser)
	closeZfs := func() {
		for _, zf := range zfs {
			zf.Close()
		}
	}
	defer closeZfs()
	open := func(p string) *zip.ReadCloser {
		if zf, ok := zfs[p]; ok {
			return zf
		}
		zf, err := zip.OpenReader(p)
		if err != nil {
			return nil
		}
		zfs[p] = zf
		return zf
	}
	var occs []occ
	for _, en := range entries {
		for _, p := range en.Paths {
			if !strings.HasSuffix(p, ".jar") {
				continue
			}
			zf := open(p)
			if zf == nil {
				continue
			}
			for _, f := range zf.File {
				if !f.FileInfo().IsDir() {
					occs = append(occs, occ{en.ID, p, f.Name})
				}
			}
		}
	}
	byName := make(map[string][]occ, len(occs))
	for _, o := range occs {
		byName[o.name] = append(byName[o.name], o)
	}
	// Group conflicting entries by their containing directory: one line per
	// package with an entry count and the jars it appears in.
	type grp struct {
		count int
		ids   []string
	}
	groups := map[string]*grp{}
	for name, list := range byName {
		if len(list) < 2 || excludedFromUber(name) {
			continue
		}
		seen := map[string]bool{}
		hashes := map[string]bool{}
		var ids []string
		for _, o := range list {
			if seen[o.id] {
				continue
			}
			seen[o.id] = true
			ids = append(ids, o.id)
			hashes[entrySHA256(open(o.file), name)] = true
		}
		if len(ids) < 2 || len(hashes) < 2 {
			continue
		}
		g := groups[pkgOf(name)]
		if g == nil {
			g = &grp{}
			groups[pkgOf(name)] = g
		}
		g.count++
		for _, id := range ids {
			found := false
			for _, gid := range g.ids {
				if gid == id {
					found = true
					break
				}
			}
			if !found {
				g.ids = append(g.ids, id)
			}
		}
	}
	if len(groups) == 0 {
		return
	}
	keys := make([]string, 0, len(groups))
	total := 0
	for k, g := range groups {
		keys = append(keys, k)
		total += g.count
	}
	sort.Strings(keys)
	fmt.Fprintf(os.Stderr, "rig: warning: %d uber entries conflict across classpath jars (first in classpath order wins):\n", total)
	for _, k := range keys {
		g := groups[k]
		plural := "entries"
		if g.count == 1 {
			plural = "entry"
		}
		fmt.Fprintf(os.Stderr, "  %s (%d %s): %s\n", k, g.count, plural, idList(g.ids))
	}
}

// idList renders a jar id list, eliding past the first 3.
func idList(ids []string) string {
	if len(ids) > 3 {
		return strings.Join(ids[:3], ", ") + fmt.Sprintf(", …and %d more", len(ids)-3)
	}
	return strings.Join(ids, ", ")
}

// pkgOf groups an entry by its containing directory; root entries keep their name.
func pkgOf(name string) string {
	if i := strings.LastIndex(name, "/"); i > 0 {
		return name[:i+1]
	}
	return name
}

func jarFileName(mod lockfile.Module) string {
	base := mod.Lib
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == "" {
		base = "app"
	}
	if mod.Version != "" {
		base += "-" + mod.Version
	}
	return base + ".jar"
}

// javacOptsOf composes the effective javac opts for a module: the manifest's
// :rig/javac-opts, with "--release <N>" prepended when the workspace pins a
// JVM (:rig/jvm) and the opts do not already control the source level.
func javacOptsOf(jvm *lockfile.JVM, opts []string) []string {
	if jvm == nil || controlsSourceLevel(opts) {
		return opts
	}
	if n := jdk.FeatureVersion(jvm.Requested); n > 0 {
		return append([]string{"--release", strconv.Itoa(n)}, opts...)
	}
	return opts
}

// controlsSourceLevel reports whether opts already set --release, -source or
// -target, which javac refuses to combine with another --release.
func controlsSourceLevel(opts []string) bool {
	for _, o := range opts {
		switch {
		case o == "--release" || strings.HasPrefix(o, "--release="):
			return true
		case o == "-source" || strings.HasPrefix(o, "-source="):
			return true
		case o == "-target" || strings.HasPrefix(o, "-target="):
			return true
		}
	}
	return false
}
