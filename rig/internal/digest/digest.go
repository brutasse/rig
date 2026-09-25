package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Dirs returns the sha256 of a content-addressed listing of the regular
// files under all given directories: one "relpath:sha256\n" line per file,
// digested in walk order (relative to each directory's root). Missing or
// non-directory roots are skipped, so optional source dirs (e.g. a
// projectless "java/") do not change the digest.
func Dirs(dirs ...string) (string, error) {
	h := sha256.New()
	for _, dir := range dirs {
		st, err := os.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if !st.IsDir() {
			continue
		}
		err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			fh, err := File(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s:%s\n", filepath.ToSlash(rel), fh)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
