package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brutasse/rig/internal/updater"
)

// noticeTTL is how long an update-check result is trusted.
const noticeTTL = 24 * time.Hour

// selfUpdated is set by 'rig self-update' so the passive notice does not
// fire in the same run that just updated.
var selfUpdated bool

// updateNotice returns a one-line "update available" notice, or "". It makes
// at most one network request per noticeTTL (the result is cached in the
// state dir) and never fails: a failed check is cached and silent.
func (o *opts) updateNotice() string {
	if Version == "dev" || o.offline || selfUpdated {
		return ""
	}
	if os.Getenv("RIG_UPDATE_CHECK") == "0" {
		return ""
	}
	store, err := o.store()
	if err != nil {
		return ""
	}
	path := filepath.Join(store.Root, "update-check")
	tag := ""
	if b, err := os.ReadFile(path); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) == 2 {
			if ts, err := strconv.ParseInt(fields[0], 10, 64); err == nil && time.Since(time.Unix(ts, 0)) < noticeTTL {
				tag = fields[1]
			}
		}
	}
	if tag == "" {
		tag = "-"
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if rel, err := updater.Latest(ctx, &http.Client{Timeout: 3 * time.Second}, apiBase()); err == nil {
			tag = rel.Tag
		}
		writeUpdateCheck(store.Root, tag)
	}
	if tag == "-" {
		return ""
	}
	if updater.CompareVersions(strings.TrimPrefix(tag, "v"), Version) > 0 {
		return fmt.Sprintf("rig %s available — run 'rig self-update'", tag)
	}
	return ""
}

// writeUpdateCheck records the last seen release tag under root (best effort).
func writeUpdateCheck(root, tag string) {
	path := filepath.Join(root, "update-check")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d %s\n", time.Now().Unix(), tag)), 0o644); err != nil {
		return
	}
}
