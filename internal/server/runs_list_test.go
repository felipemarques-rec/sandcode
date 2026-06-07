package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
)

// seedRuns drives the state cache through Apply so the test does not
// depend on the bus subscription path. Returns the run IDs in the
// order they were inserted (oldest first).
func seedRuns(t *testing.T, cache *StateCache, runs []struct {
	id       string
	terminal event.Type // empty = stays in PhaseSubmitted
}) []string {
	t.Helper()
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		cache.apply(event.New(event.RunSubmitted, r.id, nil))
		if r.terminal != "" {
			cache.apply(event.New(r.terminal, r.id, nil))
		}
		ids = append(ids, r.id)
	}
	return ids
}

func TestListRuns_EmptyCacheReturnsEmptyArray(t *testing.T) {
	srv := newRunsTestServer(t, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp ListRunsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 0 {
		t.Errorf("Runs = %d, want 0", len(resp.Runs))
	}
	// Must be the empty array, not null — clients rely on iterating.
	if resp.Runs == nil {
		t.Errorf("Runs is nil, want []")
	}
}

func TestListRuns_NewestFirst(t *testing.T) {
	srv := newRunsTestServer(t, nil, nil)
	ids := seedRuns(t, srv.opts.StateCache, []struct {
		id       string
		terminal event.Type
	}{
		{"r0000001", ""},
		{"r0000002", ""},
		{"r0000003", ""},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	var resp ListRunsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	if len(resp.Runs) != 3 {
		t.Fatalf("Runs = %d, want 3", len(resp.Runs))
	}
	// Reverse of insertion order: r3, r2, r1.
	want := []string{ids[2], ids[1], ids[0]}
	for i, st := range resp.Runs {
		if st.RunID != want[i] {
			t.Errorf("Runs[%d].RunID = %q, want %q", i, st.RunID, want[i])
		}
	}
}

func TestListRuns_LimitHonored(t *testing.T) {
	srv := newRunsTestServer(t, nil, nil)
	seedRuns(t, srv.opts.StateCache, []struct {
		id       string
		terminal event.Type
	}{
		{"r0000001", ""},
		{"r0000002", ""},
		{"r0000003", ""},
		{"r0000004", ""},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/runs?limit=2", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	var resp ListRunsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Runs) != 2 {
		t.Errorf("Runs = %d, want 2", len(resp.Runs))
	}
	// Newest-first with limit 2 → r4, r3.
	if resp.Runs[0].RunID != "r0000004" || resp.Runs[1].RunID != "r0000003" {
		t.Errorf("wrong page: %+v", resp.Runs)
	}
}

func TestListRuns_PhaseFilter(t *testing.T) {
	srv := newRunsTestServer(t, nil, nil)
	// RunFailed transitions directly from PhaseSubmitted, so we can
	// land runs in PhaseFailed with a single follow-up event. Mix
	// failed + still-submitted to verify the filter discriminates.
	seedRuns(t, srv.opts.StateCache, []struct {
		id       string
		terminal event.Type
	}{
		{"r0000001", event.RunFailed},
		{"r0000002", ""}, // stays submitted
		{"r0000003", event.RunFailed},
		{"r0000004", ""}, // stays submitted
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/runs?phase=failed", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	var resp ListRunsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("Runs = %d, want 2: %+v", len(resp.Runs), resp.Runs)
	}
	for _, st := range resp.Runs {
		if st.Phase != runtime.PhaseFailed {
			t.Errorf("Run %s phase=%s, want failed", st.RunID, st.Phase)
		}
	}
}

func TestListRuns_InvalidLimitReturns400(t *testing.T) {
	srv := newRunsTestServer(t, nil, nil)

	for _, v := range []string{"abc", "-1", "0"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/runs?limit="+v, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("limit=%q: status = %d, want 400", v, rr.Code)
		}
	}
}

func TestListRuns_LimitCapsAtMax(t *testing.T) {
	srv := newRunsTestServer(t, nil, nil)
	seedRuns(t, srv.opts.StateCache, []struct {
		id       string
		terminal event.Type
	}{
		{"r0000001", ""},
		{"r0000002", ""},
	})

	// limit way above maxListLimit — handler must clamp and still serve.
	req := httptest.NewRequest(http.MethodGet, "/v1/runs?limit=99999999", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ListRunsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Runs) != 2 {
		t.Errorf("Runs = %d, want 2", len(resp.Runs))
	}
}
