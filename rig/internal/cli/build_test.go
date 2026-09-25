package cli

import (
	"reflect"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
)

func TestJavacOptsOf(t *testing.T) {
	pin := func(requested string) *lockfile.JVM {
		return &lockfile.JVM{Vendor: "temurin", Requested: requested, Version: requested}
	}
	tests := []struct {
		name string
		jvm  *lockfile.JVM
		opts []string
		want []string
	}{
		{"no pin, opts pass through", nil, []string{"-encoding", "UTF-8"}, []string{"-encoding", "UTF-8"}},
		{"no pin, no opts", nil, nil, nil},
		{"pin, no opts", pin("17"), nil, []string{"--release", "17"}},
		{"pin with full version request", pin("17.0.13+9"), nil, []string{"--release", "17"}},
		{"pin with legacy 1.x request", pin("1.8"), nil, []string{"--release", "8"}},
		{"pin, module opts kept after injection", pin("11"), []string{"-Dfoo=bar", "-encoding", "UTF-8"}, []string{"--release", "11", "-Dfoo=bar", "-encoding", "UTF-8"}},
		{"pin, user --release wins", pin("11"), []string{"--release", "17"}, []string{"--release", "17"}},
		{"pin, user --release= wins", pin("11"), []string{"--release=17"}, []string{"--release=17"}},
		{"pin, user -source wins", pin("11"), []string{"-source", "11", "-target", "11"}, []string{"-source", "11", "-target", "11"}},
		{"pin, user -source= wins", pin("11"), []string{"-source=11"}, []string{"-source=11"}},
		{"pin, user -target wins", pin("11"), []string{"-target", "11"}, []string{"-target", "11"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := javacOptsOf(tt.jvm, tt.opts); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("javacOptsOf = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestControlsSourceLevel(t *testing.T) {
	for _, o := range [][]string{
		{"--release", "17"},
		{"--release=17"},
		{"-g", "-source", "17"},
		{"-source=17"},
		{"-target", "17"},
		{"-target=17"},
	} {
		if !controlsSourceLevel(o) {
			t.Errorf("controlsSourceLevel(%v) = false, want true", o)
		}
	}
	for _, o := range [][]string{
		nil,
		{"-encoding", "UTF-8"},
		{"-Dfoo=bar"},
		{"-Xlint:all"},
	} {
		if controlsSourceLevel(o) {
			t.Errorf("controlsSourceLevel(%v) = true, want false", o)
		}
	}
}
