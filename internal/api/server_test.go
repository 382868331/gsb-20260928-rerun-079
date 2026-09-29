package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"releaselab/internal/api"
	"releaselab/internal/engine"
	"releaselab/internal/store"
)

func TestAPIEndToEnd(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(st)
	eng.SetClockForTest(1_700_000_000_000)
	handler := api.New(eng, nil).Handler()

	post := func(path string, body any) map[string]any {
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Fatalf("%s -> %d: %s", path, rw.Code, rw.Body.String())
		}
		res := rw.Result()
		defer res.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}

	post("/api/setup", map[string]any{
		"instance_count": 4,
		"unhealthy_idx":  []int{0, 1},
		"probe_delay_ms": map[int]int{0: 200, 1: 200},
	})
	out := post("/api/releases", map[string]any{
		"target_version_id": "ver-v2.0.0",
		"batch_percent":     []int{50, 50},
		"health_wait_ms":    2000,
		"action_ms":         1000,
	})
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("missing release id")
	}

	// Advance the virtual clock through failure + rollback.
	for range make([]int, 120) {
		eng.Advance(50)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, req)
	res := rw.Result()
	defer res.Body.Close()
	var snap struct {
		Releases  []map[string]any `json:"releases"`
		Batches   []map[string]any `json:"batches"`
		Instances []map[string]any `json:"instances"`
		Versions  []map[string]any `json:"versions"`
		Events    []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Versions) != 2 {
		t.Fatalf("versions = %d, want 2 (independently stored)", len(snap.Versions))
	}
	if len(snap.Batches) < 2 {
		t.Fatalf("batches = %d, want rollout + rollback records", len(snap.Batches))
	}
	if got := snap.Releases[0]["status"]; got != "failed" {
		t.Fatalf("release status = %v, want failed+rolled-back", got)
	}
	for _, in := range snap.Instances {
		if in["current_version"] != "ver-v1.0.0" {
			t.Fatalf("instance left on %v", in["current_version"])
		}
	}

	// Cancel on a finished release must be rejected cleanly.
	creq := httptest.NewRequest(http.MethodPost, "/api/releases/"+id+"/cancel", nil)
	crw := httptest.NewRecorder()
	handler.ServeHTTP(crw, creq)
	if crw.Code != 400 {
		t.Fatalf("cancel finished release -> %d, want 400", crw.Code)
	}
}
