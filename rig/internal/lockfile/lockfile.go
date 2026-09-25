package lockfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/brutasse/rig/internal/jdk"
)

const SupportedVersion = 1

type Document struct {
	Version   int               `json:"version"`
	Tool      Tool              `json:"tool"`
	Resolver  Resolver          `json:"resolver"`
	LockedAt  time.Time         `json:"locked_at"`
	Workspace Workspace         `json:"workspace"`
	Cooldown  Cooldown          `json:"cooldown"`
	JVM       *JVM              `json:"jvm,omitempty"`
	Artifacts []Artifact        `json:"artifacts"`
	Skipped   []Skipped         `json:"skipped"`
	Modules   map[string]Module `json:"modules"`
}

type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Resolver struct {
	Lib     string `json:"lib"`
	Version string `json:"version"`
	GitSHA  string `json:"git/sha"`
}

type Workspace struct {
	Modules        []string `json:"modules"`
	ManifestSHA256 string   `json:"manifest_sha256"`
}

type Cooldown struct {
	Default string            `json:"default"`
	Repos   map[string]string `json:"repos"`
}

// JVM is the workspace's pinned JVM, from :rig/jvm in the root manifest.
// Requested is what the manifest asks for ("21"); Version is the exact
// release resolved at lock time ("21.0.12.1+1"), "" until first resolved.
type JVM struct {
	Vendor    string `json:"vendor"`
	Requested string `json:"requested"`
	Version   string `json:"version,omitempty"`
}

type Artifact struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Group       string     `json:"group"`
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	Extension   string     `json:"extension"`
	Classifier  *string    `json:"classifier"`
	Repository  string     `json:"repository,omitempty"`
	URL         string     `json:"url,omitempty"`
	SHA256      string     `json:"sha256,omitempty"`
	PublishedAt *time.Time `json:"published_at"`
	Git         *GitRef    `json:"git"`
	DepsRoot    string     `json:"deps/root,omitempty"`
	Paths       []string   `json:"paths,omitempty"`
}

type GitRef struct {
	URL string `json:"url"`
	SHA string `json:"sha"`
}

type Skipped struct {
	Coord       string     `json:"coord"`
	Version     string     `json:"version"`
	Reason      string     `json:"reason"`
	PublishedAt *time.Time `json:"published_at"`
	Cooldown    string     `json:"cooldown,omitempty"`
}

type Module struct {
	ManifestSHA256 string           `json:"manifest_sha256"`
	Lib            string           `json:"lib"`
	Version        string           `json:"version"`
	Main           string           `json:"main,omitempty"`
	PrepEnsure     []string         `json:"prep-ensure,omitempty"`
	PrepAlias      string           `json:"prep-alias,omitempty"`
	PrepFn         string           `json:"prep-fn,omitempty"`
	JVMOpts        []string         `json:"jvm-opts"`
	Paths          []string         `json:"paths,omitempty"`
	Classpath      []ClasspathEntry `json:"classpath"`
	Aliases        map[string]Alias `json:"aliases"`
	Build          Build            `json:"build"`
	Publish        Publish          `json:"publish"`
	Test           Test             `json:"test"`
}

type ClasspathEntry struct {
	Ref   *string `json:"-"`
	Local *string `json:"-"`
}

func (e *ClasspathEntry) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		e.Ref = &s
		return nil
	}
	var o struct {
		Local string `json:"local"`
	}
	if err := json.Unmarshal(b, &o); err == nil && o.Local != "" {
		e.Local = &o.Local
		return nil
	}
	return errors.New("lockfile: bad classpath entry")
}

func (e ClasspathEntry) MarshalJSON() ([]byte, error) {
	if e.Ref != nil {
		return json.Marshal(*e.Ref)
	}
	if e.Local != nil {
		return json.Marshal(map[string]string{"local": *e.Local})
	}
	return nil, errors.New("lockfile: empty classpath entry")
}

func (e ClasspathEntry) String() string {
	if e.Ref != nil {
		return *e.Ref
	}
	if e.Local != nil {
		return "local:" + *e.Local
	}
	return ""
}

type Alias struct {
	Classpath []ClasspathEntry  `json:"classpath"`
	Paths     []string          `json:"paths,omitempty"`
	JVMOpts   []string          `json:"jvm-opts"`
	Env       map[string]string `json:"env"`
	Exec      *Exec             `json:"exec"`
}

type Exec struct {
	Type string `json:"type"`
	Fn   string `json:"fn"`
}

type Build struct {
	SrcDirs     []string `json:"src-dirs"`
	JavaSrcDirs []string `json:"java-src-dirs"`
	CompileOpts []string `json:"compile-opts,omitempty"`
	ClassDir    string   `json:"class-dir"`
	NsCompile   []string `json:"ns-compile,omitempty"`
	Jar         bool     `json:"jar"`
	Uberjar     *Uberjar `json:"uberjar"`
}

type Uberjar struct {
	File string         `json:"file"`
	Main string         `json:"main"`
	Opts map[string]any `json:"opts"`
}

type Publish struct {
	Enabled      bool   `json:"enabled"`
	Repo         string `json:"repo"`
	SignReleases bool   `json:"sign-releases?"`
}

type Test struct {
	Enabled bool `json:"enabled"`
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

func Load(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("lockfile: %s: %w", path, err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("lockfile: %s: %w", path, err)
	}
	return &d, nil
}

func (d *Document) Save(path string) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func (d *Document) Validate() error {
	if d.Version != SupportedVersion {
		return fmt.Errorf("unsupported lock version %d (want %d)", d.Version, SupportedVersion)
	}
	if len(d.Modules) == 0 {
		return errors.New("no modules")
	}
	if d.JVM != nil {
		if d.JVM.Vendor != "temurin" {
			return fmt.Errorf("jvm: unsupported vendor %q", d.JVM.Vendor)
		}
		if !jdk.ValidRequested(d.JVM.Requested) {
			return fmt.Errorf("jvm: bad requested %q", d.JVM.Requested)
		}
		if d.JVM.Version != "" && !jdk.Satisfies(d.JVM.Requested, d.JVM.Version) {
			return fmt.Errorf("jvm: locked version %q does not satisfy requested %q", d.JVM.Version, d.JVM.Requested)
		}
	}
	seen := make(map[string]bool, len(d.Artifacts))
	for _, a := range d.Artifacts {
		if seen[a.ID] {
			return fmt.Errorf("duplicate artifact %q", a.ID)
		}
		seen[a.ID] = true
		switch a.Kind {
		case "mvn":
			if !sha256Re.MatchString(a.SHA256) {
				return fmt.Errorf("artifact %q: bad sha256 %q", a.ID, a.SHA256)
			}
		case "git":
			if a.Git == nil || a.Git.SHA == "" {
				return fmt.Errorf("artifact %q: git dep without git sha", a.ID)
			}
		default:
			return fmt.Errorf("artifact %q: bad kind %q", a.ID, a.Kind)
		}
	}
	for dir, m := range d.Modules {
		if !sha256Re.MatchString(m.ManifestSHA256) {
			return fmt.Errorf("module %q: bad manifest_sha256", dir)
		}
		if err := checkClasspath(dir, "classpath", m.Classpath, seen, d.Modules); err != nil {
			return err
		}
		for name, a := range m.Aliases {
			if err := checkClasspath(dir, "alias "+name, a.Classpath, seen, d.Modules); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkClasspath(dir, what string, cp []ClasspathEntry, artifacts map[string]bool, modules map[string]Module) error {
	for _, e := range cp {
		switch {
		case e.Ref != nil:
			if !artifacts[*e.Ref] {
				return fmt.Errorf("module %q %s: unknown artifact %q", dir, what, *e.Ref)
			}
		case e.Local != nil:
			if _, ok := modules[*e.Local]; !ok {
				return fmt.Errorf("module %q %s: unknown local module %q", dir, what, *e.Local)
			}
		default:
			return fmt.Errorf("module %q %s: empty classpath entry", dir, what)
		}
	}
	return nil
}
