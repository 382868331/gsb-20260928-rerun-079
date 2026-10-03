package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"releasectl/internal/api"
	"releasectl/internal/engine"
	"releasectl/internal/store"
)

func newHTTPRig(t *testing.T) (*api.Server, *engine.Service) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.Config{
		UpdateDelay: 20 * time.Millisecond, HealthDelay: 20 * time.Millisecond, RollbackDelay: 20 * time.Millisecond,
	})
	return api.NewServer(svc, st), svc
}

func do(t *testing.T, srv *api.Server, method, path string, body any, want int) []byte {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func getState(t *testing.T, srv *api.Server) store.Snapshot {
	t.Helper()
	var snap store.Snapshot
	if err := json.Unmarshal(do(t, srv, http.MethodGet, "/api/state", nil, 200), &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func tickFor(svc *engine.Service, d time.Duration) {
	now := time.Now()
	for deadline := now.Add(d); time.Now().Before(deadline); {
		if err := svc.Engine().Tick(time.Now()); err != nil {
			panic(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Full page flow over HTTP: init the drill, configure the zone capacity
// entry, prepare two plans concurrently — exactly one gets a queryable
// commitment while the other shows its waiting reason — then a zone-level
// failure rolls back and releases the commitment from real instance states.
func TestZoneCapacityFlowOverHTTP(t *testing.T) {
	srv, svc := newHTTPRig(t)
	do(t, srv, http.MethodPost, "/api/environment/init", nil, 200)

	snap := getState(t, srv)
	if len(snap.Zones) != 2 || len(snap.Policies) != 2 {
		t.Fatalf("init must seed 2 zones and 2 capacity entries, got %d/%d", len(snap.Zones), len(snap.Policies))
	}
	zoneOf := map[string]string{}
	for _, inst := range snap.Instances {
		zoneOf[inst.ID] = inst.ZoneID
	}
	if zoneOf["inst-1"] != "zone-a" || zoneOf["inst-6"] != "zone-b" {
		t.Fatalf("seeded instances must be spread across zones: %v", zoneOf)
	}

	// Configure the zone capacity entry on the page: shared pool of 1 in
	// zone-a, so two one-instance plans cannot both occupy it.
	do(t, srv, http.MethodPost, "/api/zones/zone-a/capacity",
		map[string]any{"minAvailable": map[string]int{"svc-order": 1}, "sharedMax": 1}, 200)
	// A second service to run the competing plan.
	do(t, srv, http.MethodPost, "/api/services/svc-pay/instances", nil, 200)

	do(t, srv, http.MethodPost, "/api/releases", map[string]any{
		"serviceId": "svc-order", "targetVersion": "v2.0.0", "batchSize": 1, "strategy": "rolling",
		"failure": map[string]any{"mode": "none"},
	}, 200)
	do(t, srv, http.MethodPost, "/api/releases", map[string]any{
		"serviceId": "svc-pay", "targetVersion": "v2.0.0", "batchSize": 1, "strategy": "rolling",
		"failure": map[string]any{"mode": "none"},
	}, 200)

	// While the first plan occupies zone-a, the second must show its waiting
	// reason; poll because the window closes once the first plan moves on.
	waiting := 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if err := svc.Engine().Tick(time.Now()); err != nil {
			t.Fatal(err)
		}
		snap = getState(t, srv)
		waiting = 0
		for _, b := range snap.Batches {
			if b.CapacityWaiting != "" {
				waiting++
			}
		}
		if waiting > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	held := 0
	for _, r := range snap.Reservations {
		if r.State == "held" && r.ZoneID == "zone-a" {
			held++
			if len(r.Basis) == 0 {
				t.Fatal("held commitment must carry its per-instance basis")
			}
		}
	}
	if held != 1 {
		t.Fatalf("exactly one zone-a commitment may be held, got %d", held)
	}
	if waiting == 0 {
		t.Fatal("the losing plan must show a persisted waiting reason")
	}

	// Both plans finish eventually; every commitment is released with
	// evidence taken from the real instance records.
	tickFor(svc, 5*time.Second)
	snap = getState(t, srv)
	for _, r := range snap.Releases {
		if r.Status != "succeeded" {
			t.Fatalf("release %s = %s, want succeeded", r.ID, r.Status)
		}
	}
	for _, res := range snap.Reservations {
		if res.State != "released" || res.ReleaseReason == "" || len(res.ReleaseBasis) == 0 {
			t.Fatalf("commitment %s must be released with evidence, got %s", res.ID, res.State)
		}
	}
}

// An incompatible rollback: the half-batch health failure rolls the batch
// back, and the zone commitment is released only once the instances are
// really back on the baseline.
func TestZoneCapacityReleasedAfterRollbackHTTP(t *testing.T) {
	srv, svc := newHTTPRig(t)
	do(t, srv, http.MethodPost, "/api/environment/init", nil, 200)
	do(t, srv, http.MethodPost, "/api/zones/zone-a/capacity",
		map[string]any{"minAvailable": map[string]int{"svc-order": 1}, "sharedMax": 0}, 200)
	do(t, srv, http.MethodPost, "/api/releases", map[string]any{
		"serviceId": "svc-order", "targetVersion": "v2.0.0", "batchSize": 2, "strategy": "rolling",
		"failure": map[string]any{"mode": "half_batch"},
	}, 200)
	tickFor(svc, 5*time.Second)
	snap := getState(t, srv)
	var rel store.Release
	for _, r := range snap.Releases {
		rel = r
	}
	if rel.Status != "rolled_back" {
		t.Fatalf("release = %s, want rolled_back", rel.Status)
	}
	if len(snap.Reservations) == 0 {
		t.Fatal("expected a capacity commitment for the expanded batch")
	}
	for _, res := range snap.Reservations {
		if res.State != "released" {
			t.Fatalf("commitment %s still held after rollback", res.ID)
		}
	}
	for _, inst := range snap.Instances {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s = %s, want baseline after rollback", inst.ID, inst.CurrentVersion)
		}
	}
}
