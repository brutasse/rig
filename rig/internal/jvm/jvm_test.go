package jvm

import (
	"strings"
	"testing"
)

func TestFind(t *testing.T) {
	if _, err := Find(); err != nil {
		t.Skipf("no java in test environment: %v", err)
	}
}

func TestVersion(t *testing.T) {
	java, err := Find()
	if err != nil {
		t.Skipf("no java in test environment: %v", err)
	}
	v, err := Version(java)
	if err != nil {
		t.Fatal(err)
	}
	if v == "" || strings.Count(v, ".") < 1 {
		t.Errorf("version = %q", v)
	}
}
