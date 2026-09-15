package maven

import (
	"os"
	"path/filepath"
	"testing"
)

const namespaced = `<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.1.0"
          xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
          xsi:schemaLocation="http://maven.apache.org/SETTINGS/1.1.0 https://maven.apache.org/xsd/settings-1.1.0.xsd">
  <localRepository>/custom/m2</localRepository>
  <servers>
    <server>
      <id>corp</id>
      <username>alice</username>
      <password>s3cret</password>
    </server>
    <server>
      <id>central</id>
      <username>bob</username>
    </server>
    <server>
      <id>empty</id>
    </server>
  </servers>
</settings>`

const plain = `<?xml version="1.0"?>
<settings>
  <servers>
    <server>
      <id>corp</id>
      <username>alice</username>
      <password>s3cret</password>
    </server>
  </servers>
</settings>`

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.xml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadNamespaced(t *testing.T) {
	s, err := Load(writeSettings(t, namespaced))
	if err != nil {
		t.Fatal(err)
	}
	if s.LocalRepo != "/custom/m2" {
		t.Errorf("LocalRepo = %q", s.LocalRepo)
	}
	if len(s.Servers) != 3 {
		t.Fatalf("len(Servers) = %d, want 3", len(s.Servers))
	}
	if srv := s.Servers["corp"]; srv.Username != "alice" || srv.Password != "s3cret" {
		t.Errorf("corp = %+v", srv)
	}
	if srv := s.Servers["central"]; srv.Username != "bob" || srv.Password != "" {
		t.Errorf("central = %+v", srv)
	}
	if _, ok := s.Servers["empty"]; !ok {
		t.Error("server without credentials missing")
	}
}

func TestLoadPlain(t *testing.T) {
	s, err := Load(writeSettings(t, plain))
	if err != nil {
		t.Fatal(err)
	}
	if s.LocalRepo != "" {
		t.Errorf("LocalRepo = %q, want empty", s.LocalRepo)
	}
	if srv := s.Servers["corp"]; srv.Username != "alice" || srv.Password != "s3cret" {
		t.Errorf("corp = %+v", srv)
	}
}

func TestLoadMissing(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.xml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if s.LocalRepo != "" || len(s.Servers) != 0 {
		t.Errorf("s = %+v, want zero", s)
	}
}
