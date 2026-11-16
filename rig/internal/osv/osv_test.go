package osv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func batchServer(t *testing.T, handle func(w http.ResponseWriter, req []Query)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/querybatch" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Queries []Query `json:"queries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		handle(w, body.Queries)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeResults(w http.ResponseWriter, queries []Query, vulns func(i int) []string) {
	results := make([]map[string]any, len(queries))
	for i := range queries {
		ids := make([]map[string]string, 0)
		for _, id := range vulns(i) {
			ids = append(ids, map[string]string{"id": id, "modified": "2024-01-01T00:00:00Z"})
		}
		results[i] = map[string]any{"vulns": ids}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
}

func TestQueryBatch(t *testing.T) {
	srv := batchServer(t, func(w http.ResponseWriter, queries []Query) {
		writeResults(w, queries, func(i int) []string {
			if queries[i].Version == "1.2.3" {
				return []string{"GHSA-abc", "GHSA-def"}
			}
			return nil
		})
	})
	ids, err := QueryBatch(context.Background(), srv.Client(), srv.URL, []Query{
		{Package: Pkg{Name: "g:a", Ecosystem: "Maven"}, Version: "1.0.0"},
		{Package: Pkg{Name: "g:a", Ecosystem: "Maven"}, Version: "1.2.3"},
		{Package: Pkg{Name: "g:b", Ecosystem: "Maven"}, Version: "2.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("got %d results, want 3", len(ids))
	}
	if len(ids[0]) != 0 || len(ids[2]) != 0 {
		t.Errorf("clean queries not clean: %v", ids)
	}
	if len(ids[1]) != 2 || ids[1][0] != "GHSA-abc" || ids[1][1] != "GHSA-def" {
		t.Errorf("vuln ids = %v, want [GHSA-abc GHSA-def]", ids[1])
	}
}

// The API caps a batch at MaxBatchQueries: more queries must go out in
// successive requests, and every result lands at its query index.
func TestQueryBatchChunking(t *testing.T) {
	var requestSizes []int
	srv := batchServer(t, func(w http.ResponseWriter, queries []Query) {
		requestSizes = append(requestSizes, len(queries))
		writeResults(w, queries, func(i int) []string { return nil })
	})
	queries := make([]Query, MaxBatchQueries+1)
	for i := range queries {
		queries[i] = Query{Package: Pkg{Name: "g:a", Ecosystem: "Maven"}, Version: "1.0.0"}
	}
	ids, err := QueryBatch(context.Background(), srv.Client(), srv.URL, queries)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(queries) {
		t.Fatalf("got %d results, want %d", len(ids), len(queries))
	}
	for i, id := range ids {
		if len(id) != 0 {
			t.Fatalf("query %d not clean: %v", i, id)
		}
	}
	if len(requestSizes) != 2 || requestSizes[0] != MaxBatchQueries || requestSizes[1] != 1 {
		t.Errorf("request sizes = %v, want [%d 1]", requestSizes, MaxBatchQueries)
	}
}

// A result carrying a next_page_token must be re-queried with that token
// — and only that query — until the token stops coming back.
func TestQueryBatchPagination(t *testing.T) {
	var requests [][]Query
	srv := batchServer(t, func(w http.ResponseWriter, queries []Query) {
		requests = append(requests, append([]Query(nil), queries...))
		results := make([]map[string]any, len(queries))
		for i, q := range queries {
			ids := []string{}
			// Only the vuln query paginates: first page, then the page
			// the token continues.
			if q.Package.Name == "g:vuln" {
				switch q.PageToken {
				case "":
					ids = []string{"GHSA-first"}
				case "page2":
					ids = []string{"GHSA-second"}
				}
			}
			vulns := make([]map[string]string, len(ids))
			for j, id := range ids {
				vulns[j] = map[string]string{"id": id}
			}
			r := map[string]any{"vulns": vulns}
			if q.Package.Name == "g:vuln" && q.PageToken == "" {
				r["next_page_token"] = "page2"
			}
			results[i] = r
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	ids, err := QueryBatch(context.Background(), srv.Client(), srv.URL, []Query{
		{Package: Pkg{Name: "g:clean", Ecosystem: "Maven"}, Version: "1.0.0"},
		{Package: Pkg{Name: "g:vuln", Ecosystem: "Maven"}, Version: "1.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids[0]) != 0 {
		t.Errorf("clean query not clean: %v", ids[0])
	}
	if len(ids[1]) != 2 || ids[1][0] != "GHSA-first" || ids[1][1] != "GHSA-second" {
		t.Errorf("paginated ids = %v, want [GHSA-first GHSA-second]", ids[1])
	}
	if len(requests) != 2 {
		t.Fatalf("%d requests, want 2: %+v", len(requests), requests)
	}
	if len(requests[1]) != 1 || requests[1][0].PageToken != "page2" ||
		requests[1][0].Package.Name != "g:vuln" {
		t.Errorf("second request = %+v, want the one paginated query with its token", requests[1])
	}
}

func TestQueryBatchErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad thing", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := QueryBatch(context.Background(), srv.Client(), srv.URL, []Query{{Package: Pkg{Name: "g:a"}}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v, want an HTTP 500 report", err)
	}
}

func TestQueryBatchMisalignedResponse(t *testing.T) {
	srv := batchServer(t, func(w http.ResponseWriter, queries []Query) {
		// One fewer result than query: the API would not do this, but a
		// misaligned response must not be read as if it were aligned.
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"vulns": []any{}}}})
	})
	_, err := QueryBatch(context.Background(), srv.Client(), srv.URL, []Query{
		{Package: Pkg{Name: "g:a"}}, {Package: Pkg{Name: "g:b"}},
	})
	if err == nil || !strings.Contains(err.Error(), "1 results for 2 queries") {
		t.Fatalf("err = %v, want a misalignment report", err)
	}
}

func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vulns/GHSA-test-id" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "GHSA-test-id",
			"summary": "A summary",
			"aliases": ["CVE-2024-0001"],
			"severity": [{"type": "CVSS_V3", "score": "9.8"}],
			"published": "2024-01-02T03:04:05Z",
			"affected": [{
				"ranges": [
					{"type": "GIT", "events": [{"introduced": "0"}, {"fixed": "abcdef"}]},
					{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.11.1"}]}
				]
			}]
		}`))
	}))
	defer srv.Close()
	v, err := Get(context.Background(), srv.Client(), srv.URL, "GHSA-test-id")
	if err != nil {
		t.Fatal(err)
	}
	if v.Summary != "A summary" || len(v.Aliases) != 1 || v.Aliases[0] != "CVE-2024-0001" ||
		v.Published != "2024-01-02T03:04:05Z" ||
		len(v.Severity) != 1 || v.Severity[0].Type != "CVSS_V3" || v.Severity[0].Score != "9.8" {
		t.Errorf("v = %+v", v)
	}
}

func TestFixedVersions(t *testing.T) {
	v := &Vulnerability{Affected: []Affected{{Ranges: []Range{
		{Type: "GIT", Events: []Event{{Fixed: "abcdef12"}}},
		{Type: "ECOSYSTEM", Events: []Event{{Introduced: "0"}, {Fixed: "1.11.1"}, {Introduced: "2.0.0"}}},
		{Type: "SEMVER", Events: []Event{{Fixed: "1.11.1"}, {Fixed: "1.12.0"}}},
	}}}}
	got := FixedVersions(v)
	if len(got) != 2 || got[0] != "1.11.1" || got[1] != "1.12.0" {
		t.Errorf("FixedVersions = %v, want [1.11.1 1.12.0] (GIT sha and the duplicate dropped)", got)
	}
}

func TestGetNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := Get(context.Background(), srv.Client(), srv.URL, "GHSA-missing")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v, want an HTTP 404 report", err)
	}
}
