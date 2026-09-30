package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"releasectl/internal/api"
	"releasectl/internal/engine"
	"releasectl/internal/store"
)

type env struct {
	mux http.Handler
	svc *engine.Service
}

func newTestEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.Config{
		UpdateDelay: 50 * time.Millisecond, HealthDelay: 50 * time.Millisecond, RollbackDelay: 50 * time.Millisecond,
	})
	if err := svc.Seed(time.Now()); err != nil {
		t.Fatal(err)
	}

	// Lay down a built index.html so the SPA entrypoint is served.
	static := filepath.Join(dir, "dist")
	if err := os.MkdirAll(static, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "index.html"), []byte("<!doctype html><div id=root></div>"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := api.NewServer(svc, st)
	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Routes())
	mux.Handle("/", http.FileServer(http.Dir(static)))
	return &env{mux: mux, svc: svc}
}

func (e *env) do(t *testing.T, method, target, body string) (int, []byte) {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, target, reader)
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (e *env) tickUntil(t *testing.T, wantRelStatus string, timeout time.Duration) store.Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := e.svc.Engine().Tick(time.Now()); err != nil {
			t.Fatal(err)
		}
		_, data := e.do(t, http.MethodGet, "/api/state", "")
		var snap store.Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatal(err)
		}
		if wantRelStatus == "" {
			return snap
		}
		for _, rr := range snap.Releases {
			if rr.Status == wantRelStatus {
				return snap
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("release never reached %s", wantRelStatus)
	return store.Snapshot{}
}

func TestSnapshotAndSPA(t *testing.T) {
	e := newTestEnv(t)
	code, data := e.do(t, http.MethodGet, "/api/state", "")
	if code != 200 {
		t.Fatalf("state status = %d", code)
	}
	var snap struct {
		Instances []store.Instance `json:"instances"`
		Versions  []store.Version  `json:"versions"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Instances) != 6 || len(snap.Versions) != 2 {
		t.Fatalf("seed mismatch: %d instances %d versions", len(snap.Instances), len(snap.Versions))
	}

	code, data = e.do(t, http.MethodGet, "/", "")
	if code != 200 {
		t.Fatalf("index status = %d", code)
	}
	if !strings.Contains(string(data), "id=root") {
		t.Fatal("SPA entrypoint not served")
	}
}

func TestPausedReleaseHTTPFlow(t *testing.T) {
	e := newTestEnv(t)
	code, body := e.do(t, http.MethodPost, "/api/releases", `{"targetVersion":"v2.0.0","batchSize":2,"strategy":"rolling","failure":{"mode":"half_batch","pauseOnFailure":true}}`)
	var rel struct {
		ID string `json:"id"`
	}
	if code != 200 {
		t.Fatalf("create status = %d body=%s", code, body)
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		t.Fatal(err)
	}
	if rel.ID == "" {
		t.Fatal("missing release id")
	}
	e.tickUntil(t, "paused", 5*time.Second)

	code, _ = e.do(t, http.MethodPost, "/api/releases/"+rel.ID+"/resume", "")
	if code != 202 {
		t.Fatalf("resume status = %d", code)
	}
	snap := e.tickUntil(t, "rolled_back", 5*time.Second)
	for _, inst := range snap.Instances {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s on %s after resumed rollback", inst.ID, inst.CurrentVersion)
		}
	}
}

// A fresh data directory must serve empty collections as [] (never null),
// so the UI renders an explicit empty state instead of crashing.
func TestEmptySnapshotUsesEmptyArrays(t *testing.T) {
	e := newTestEnv(t)
	code, data := e.do(t, http.MethodGet, "/api/state", "")
	if code != 200 {
		t.Fatalf("state status = %d", code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"releases", "batches", "ops", "events", "versions", "instances"} {
		v := strings.TrimSpace(string(raw[key]))
		if v == "null" || v == "" {
			t.Fatalf("%s serialized as %q, want [] for empty collections", key, v)
		}
	}
	if got := strings.TrimSpace(string(raw["releases"])); got != "[]" {
		t.Fatalf("releases = %s, want [] on a fresh directory", got)
	}
}

// Review pause over HTTP: pause a healthy release, inspect the evidence via
// the snapshot, then continue to a successful finish.
func TestReviewPauseHTTPFlow(t *testing.T) {
	e := newTestEnv(t)
	code, body := e.do(t, http.MethodPost, "/api/releases", `{"targetVersion":"v2.0.0","batchSize":2,"strategy":"rolling","failure":{"mode":"none"}}`)
	if code != 200 {
		t.Fatalf("create status = %d body=%s", code, body)
	}
	var rel struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		t.Fatal(err)
	}
	code, _ = e.do(t, http.MethodPost, "/api/releases/"+rel.ID+"/pause", "")
	if code != 202 {
		t.Fatalf("pause status = %d", code)
	}
	snap := e.tickUntil(t, "paused", 5*time.Second)
	var pausedBatch *store.Batch
	for i := range snap.Batches {
		if snap.Batches[i].Phase == "paused" {
			pausedBatch = &snap.Batches[i]
		}
	}
	if pausedBatch == nil {
		t.Fatal("no paused batch after review pause")
	}
	for _, m := range pausedBatch.Members {
		if m.HealthState != "healthy" {
			t.Fatalf("review evidence incomplete: %s = %s", m.InstanceID, m.HealthState)
		}
	}
	code, _ = e.do(t, http.MethodPost, "/api/releases/"+rel.ID+"/continue", "")
	if code != 202 {
		t.Fatalf("continue status = %d", code)
	}
	snap = e.tickUntil(t, "succeeded", 10*time.Second)
	for _, inst := range snap.Instances {
		if inst.CurrentVersion != "v2.0.0" {
			t.Fatalf("instance %s on %s after continue", inst.ID, inst.CurrentVersion)
		}
	}
}
