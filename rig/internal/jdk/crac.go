package jdk

import (
	"context"
	"fmt"
	"runtime"
)

// CRaCVendor namespaces the checkpoint JVM in the JDK store. Its installs
// are deliberately invisible to List and Best: a CRaC build must never
// satisfy a workspace's JVM request — only the kernel's checkpoint
// machinery (internal/kernelrun) selects it.
const CRaCVendor = "zulu-crac"

// CRaCPins pins the exact CRaC build rig installs, per platform: version,
// URL and sha256. Unlike Temurin (resolved live through the Adoptium API),
// this is a static pin: the checkpoint image key tracks the JDK build, so
// channel drift would silently strand every image; upgrades are an
// explicit rig change. Both builds were checksum-verified against Azul's
// package metadata and the downloaded artifact. Overridable for tests and
// mirrors (same convention as DefaultBase).
var CRaCPins = map[string]Asset{
	"linux/amd64": {
		Vendor:  CRaCVendor,
		Version: "25.0.4+1",
		OS:      "linux", Arch: "x64",
		Archive: "zulu25.36.205-ca-crac-jdk25.0.4.1-linux_x64.tar.gz",
		URL:     "https://cdn.azul.com/zulu/bin/zulu25.36.205-ca-crac-jdk25.0.4.1-linux_x64.tar.gz",
		SHA256:  "6d1beee9795c544f53ff7e665d6455081960d18b476c0f6054f5e35cef97b61e",
		Size:    238337000,
	},
	"linux/arm64": {
		Vendor:  CRaCVendor,
		Version: "25.0.4+1",
		OS:      "linux", Arch: "aarch64",
		Archive: "zulu25.36.205-ca-crac-jdk25.0.4.1-linux_aarch64.tar.gz",
		URL:     "https://cdn.azul.com/zulu/bin/zulu25.36.205-ca-crac-jdk25.0.4.1-linux_aarch64.tar.gz",
		SHA256:  "cec6bb580db329e9616b63294221d4d817a4b3921d2bc1d092ff3c5ba471de7e",
		Size:    237425000,
	},
}

// PinnedCRaC returns the CRaC asset pinned for this platform.
func PinnedCRaC() (Asset, bool) {
	a, ok := CRaCPins[runtime.GOOS+"/"+runtime.GOARCH]
	return a, ok
}

// CRaCInstalled returns the pinned CRaC JDK when the store already holds
// it, else ErrNotInstalled. Lookup only — the download runs behind the
// explicit 'rig crac install' (EnsureCRaC), never from a kernel call.
func CRaCInstalled(st *Store) (*Inst, error) {
	a, ok := PinnedCRaC()
	if !ok {
		return nil, ErrNotInstalled
	}
	return st.LookupVendor(CRaCVendor, a.Version)
}

// EnsureCRaC installs the pinned CRaC build (download, sha256 verify,
// extract). Idempotent; concurrent installs serialize through the store.
func EnsureCRaC(ctx context.Context, st *Store) (*Inst, error) {
	a, ok := PinnedCRaC()
	if !ok {
		return nil, fmt.Errorf("jdk: no CRaC JDK pinned for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return st.Install(ctx, a)
}
