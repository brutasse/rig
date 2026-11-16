// Package osv queries the OSV.dev API for known vulnerabilities of
// pinned package versions.
package osv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DefaultAPI is the base of the OSV.dev v1 API (no trailing slash).
const DefaultAPI = "https://api.osv.dev/v1"

// MaxBatchQueries is the maximum number of queries one batch request may
// carry: the OSV API refuses larger batches.
const MaxBatchQueries = 1000

// Pkg is an OSV package reference. For the Maven ecosystem, Name is
// "group:artifact".
type Pkg struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem,omitempty"`
}

// Query asks whether one package version has known vulnerabilities.
// PageToken continues a paginated query; it is managed by QueryBatch,
// callers leave it empty.
type Query struct {
	Package   Pkg    `json:"package"`
	Version   string `json:"version"`
	PageToken string `json:"page_token,omitempty"`
}

type batchRequest struct {
	Queries []Query `json:"queries"`
}

// VulnRef is the batch response's per-query entry: only the id and
// modified field, the full record comes from Get.
type VulnRef struct {
	ID       string `json:"id"`
	Modified string `json:"modified"`
}

type batchResult struct {
	Vulns         []VulnRef `json:"vulns"`
	NextPageToken string    `json:"next_page_token"`
}

type batchResponse struct {
	Results []batchResult `json:"results"`
}

// QueryBatch asks the OSV API which of the queries have known
// vulnerabilities and returns the vulnerability ids per query index
// (an empty slice: the query is clean). Queries beyond MaxBatchQueries
// are sent in successive batch requests, and a per-query pagination
// token is followed until exhausted — the API paginates a query past
// 1000 matching vulnerabilities.
func QueryBatch(ctx context.Context, client *http.Client, base string, queries []Query) ([][]string, error) {
	ids := make([][]string, len(queries))
	type pending struct {
		query Query
		index int // position in queries: the batch response aligns to the
		// request, and a page re-requests only a subset of the queries,
		// so the original index must travel with each pending query.
	}
	for start := 0; start < len(queries); start += MaxBatchQueries {
		end := start + MaxBatchQueries
		if end > len(queries) {
			end = len(queries)
		}
		pend := make([]pending, 0, end-start)
		for i := start; i < end; i++ {
			pend = append(pend, pending{query: queries[i], index: i})
		}
		for {
			reqs := make([]Query, len(pend))
			for i, p := range pend {
				reqs[i] = p.query
			}
			var resp batchResponse
			if err := post(ctx, client, base+"/querybatch", batchRequest{Queries: reqs}, &resp); err != nil {
				return nil, err
			}
			if len(resp.Results) != len(pend) {
				return nil, fmt.Errorf("osv: batch response has %d results for %d queries", len(resp.Results), len(pend))
			}
			next := make([]pending, 0, len(pend))
			for i, r := range resp.Results {
				for _, v := range r.Vulns {
					ids[pend[i].index] = append(ids[pend[i].index], v.ID)
				}
				if r.NextPageToken != "" {
					p := pend[i]
					p.query.PageToken = r.NextPageToken
					next = append(next, p)
				}
			}
			if len(next) == 0 {
				break
			}
			pend = next
		}
	}
	return ids, nil
}

// Vulnerability is the OSV record of one vulnerability, as returned by
// Get. Only the fields the audit report needs are decoded.
type Vulnerability struct {
	ID        string     `json:"id"`
	Summary   string     `json:"summary"`
	Aliases   []string   `json:"aliases"`
	Severity  []Severity `json:"severity"`
	Published string     `json:"published"`
	Affected  []Affected `json:"affected"`
}

type Severity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

type Affected struct {
	Ranges []Range `json:"ranges"`
}

type Range struct {
	Type   string  `json:"type"`
	Events []Event `json:"events"`
}

// Event is one bound of a version range: introduced or fixed, mutually
// exclusive per event.
type Event struct {
	Introduced string `json:"introduced"`
	Fixed      string `json:"fixed"`
}

// Get fetches the full record of one vulnerability.
func Get(ctx context.Context, client *http.Client, base, id string) (*Vulnerability, error) {
	u := base + "/vulns/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("osv: %s: HTTP %d: %s", u, resp.StatusCode, snippet(raw))
	}
	var v Vulnerability
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("osv: %s: bad response: %w", u, err)
	}
	return &v, nil
}

// FixedVersions returns the versions that fix v, taken from its
// versioned ranges. GIT ranges are commit shas, not versions, and are
// skipped. The order of the record is preserved; duplicates are dropped.
func FixedVersions(v *Vulnerability) []string {
	seen := make(map[string]bool)
	var out []string
	for _, a := range v.Affected {
		for _, r := range a.Ranges {
			if r.Type == "GIT" {
				continue
			}
			for _, e := range r.Events {
				if e.Fixed != "" && !seen[e.Fixed] {
					seen[e.Fixed] = true
					out = append(out, e.Fixed)
				}
			}
		}
	}
	return out
}

// post issues a JSON POST to u, decodes the 200 response into out, and
// reports any other status as an error naming the response body.
func post(ctx context.Context, client *http.Client, u string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("osv: %s: HTTP %d: %s", u, resp.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("osv: %s: bad response: %w", u, err)
	}
	return nil
}

// snippet shortens a response body for an error message.
func snippet(raw []byte) string {
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return strings.TrimSpace(string(raw))
}
