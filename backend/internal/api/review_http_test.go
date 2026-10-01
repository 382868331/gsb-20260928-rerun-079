package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"releasectl/internal/api"
	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// An untouched data directory must serve an explicit empty snapshot; the
// environment is initialized on demand and no release is pre-written.
func TestEmptyStateThenInit(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.DefaultConfig())
	srv := api.NewServer(svc, st)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("state = %d", rec.Code)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Versions) != 0 || len(snap.Instances) != 0 || len(snap.Releases) != 0 {
		t.Fatalf("fresh directory must be empty, got %d/%d/%d", len(snap.Versions), len(snap.Instances), len(snap.Releases))
	}

	req = httptest.NewRequest(http.MethodPost, "/api/environment/init", nil)
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("init = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Versions) != 2 || len(snap.Instances) != 6 || len(snap.Releases) != 0 {
		t.Fatalf("after init want 2 versions/6 instances/0 releases, got %d/%d/%d",
			len(snap.Versions), len(snap.Instances), len(snap.Releases))
	}
}

func TestPauseReviewShrinkHTTP(t *testing.T) {
	e := newTestEnv(t)
	code, body := e.do(t, http.MethodPost, "/api/releases",
		`{"targetVersion":"v2.0.0","batchSize":3,"strategy":"rolling","failure":{"mode":"none"}}`)
	if code != 200 {
		t.Fatalf("create = %d %s", code, body)
	}
	var rel struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		t.Fatal(err)
	}
	// Drive the virtual clock until the first batch's instances are updated,
	// then pause before the second batch expands.
	deadline := time.Now().Add(3 * time.Second)
	updated := false
	for time.Now().Before(deadline) && !updated {
		_ = e.svc.Engine().Tick(time.Now())
		_, data := e.do(t, http.MethodGet, "/api/state", "")
		var s store.Snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		for _, b := range s.Batches {
			if b.Index == 0 {
				all := len(b.Members) > 0
				for _, m := range b.Members {
					if m.UpdateState != "succeeded" {
						all = false
					}
				}
				updated = all
			}
			if b.Index == 1 && b.Phase != "pending" {
				t.Fatalf("second batch already expanded: %s", b.Phase)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !updated {
		t.Fatal("first batch never updated")
	}
	if code, _ = e.do(t, http.MethodPost, "/api/releases/"+rel.ID+"/pause", ""); code != 202 {
		t.Fatalf("pause = %d", code)
	}
	snap := e.tickUntil(t, "paused", 3*time.Second)
	if snap.Releases[0].ReviewAction != "" {
		t.Fatal("no review decision before the operator chooses")
	}
	if code, _ = e.do(t, http.MethodPost, "/api/releases/"+rel.ID+"/review/shrink", `{"batchSize":1}`); code != 202 {
		t.Fatalf("shrink = %d", code)
	}
	snap = e.tickUntil(t, "succeeded", 5*time.Second)
	if len(snap.Batches) != 4 {
		t.Fatalf("batches = %d, want 4", len(snap.Batches))
	}
	for _, inst := range snap.Instances {
		if inst.CurrentVersion != "v2.0.0" {
			t.Fatalf("instance %s = %s", inst.ID, inst.CurrentVersion)
		}
	}
}
