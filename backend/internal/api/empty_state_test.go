package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"releasectl/internal/api"
	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// First open on a completely empty data directory must serve empty arrays
// (never null), so the page renders the empty state instead of crashing on
// a null lookup; init then seeds the drill and the state survives a reopen.
func TestEmptyStateInitAndReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.DefaultConfig())
	srv := api.NewServer(svc, st)

	get := func() map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("state code = %d", rec.Code)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	m := get()
	for _, k := range []string{"versions", "services", "rules", "instances", "releases", "batches", "ops", "events", "dependencies"} {
		if string(m[k]) != "[]" {
			t.Fatalf("empty %s = %s, want []", k, m[k])
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/environment/init", nil)
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("init code = %d", rec.Code)
	}
	m = get()
	var services, instances []map[string]any
	if err := json.Unmarshal(m["services"], &services); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(m["instances"], &instances); err != nil {
		t.Fatal(err)
	}
	if len(services) != 3 || len(instances) != 6 {
		t.Fatalf("seeded services=%d instances=%d, want 3 and 6", len(services), len(instances))
	}

	// Reopen (process restart): the seeded environment is still there.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc2 := engine.NewService(st2, engine.DefaultConfig())
	srv2 := api.NewServer(svc2, st2)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	srv2.Routes().ServeHTTP(rec2, req2)
	var m2 map[string]json.RawMessage
	if err := json.Unmarshal(rec2.Body.Bytes(), &m2); err != nil {
		t.Fatal(err)
	}
	var inst2 []map[string]any
	if err := json.Unmarshal(m2["instances"], &inst2); err != nil {
		t.Fatal(err)
	}
	if len(inst2) != 6 {
		t.Fatalf("after reopen instances = %d, want 6", len(inst2))
	}
}
