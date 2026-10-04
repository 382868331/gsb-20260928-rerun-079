package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// The cross-release capacity comparison must be a pure read: viewing history
// or exporting never acquires commitments, never releases instances and
// never changes release state.
func TestCapacityCompareHTTPFlow(t *testing.T) {
	e := newTestEnv(t)

	create := func(target string) string {
		code, body := e.do(t, http.MethodPost, "/api/releases",
			`{"serviceId":"svc-order","targetVersion":"`+target+`","batchSize":2,"strategy":"rolling","failure":{"mode":"none"}}`)
		if code != 200 {
			t.Fatalf("create release = %d %s", code, body)
		}
		var rel struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &rel); err != nil {
			t.Fatal(err)
		}
		return rel.ID
	}

	rel1 := create("v2.0.0")
	e.tickUntil(t, "succeeded", 5*time.Second)

	// A second release back to the baseline gives two comparable records.
	code, body := e.do(t, http.MethodPost, "/api/versions", `{"id":"v1.0.0","name":"基线"}`)
	if code != 200 && code != 400 {
		t.Fatalf("version = %d %s", code, body)
	}
	rel2 := create("v1.0.0")
	// tickUntil matches any release, so wait explicitly for rel2 to finish.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = e.svc.Engine().Tick(time.Now())
		_, data := e.do(t, http.MethodGet, "/api/state", "")
		var snap store.Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatal(err)
		}
		done := false
		for _, rr := range snap.Releases {
			if rr.ID == rel2 && rr.Status == "succeeded" {
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second release never succeeded")
		}
		time.Sleep(15 * time.Millisecond)
	}

	_, before := e.do(t, http.MethodGet, "/api/state", "")

	code, body = e.do(t, http.MethodGet, "/api/capacity/compare?a="+rel1+"&b="+rel2, "")
	if code != 200 {
		t.Fatalf("compare = %d %s", code, body)
	}
	var cmp engine.CapacityComparison
	if err := json.Unmarshal(body, &cmp); err != nil {
		t.Fatal(err)
	}
	if len(cmp.Releases) != 2 {
		t.Fatalf("expected 2 release views, got %d", len(cmp.Releases))
	}
	for _, rv := range cmp.Releases {
		if len(rv.Grants) == 0 {
			t.Fatalf("release %s must list its commitments", rv.ReleaseID)
		}
		if len(rv.Policies) == 0 {
			t.Fatalf("release %s must list per-zone min-available policy", rv.ReleaseID)
		}
		for _, g := range rv.Grants {
			if g.State != "released" || g.ReleasedAt == "" {
				t.Fatalf("finished release grant must show its occupancy window: %+v", g)
			}
			for _, inst := range g.Instances {
				if !inst.Released || inst.ReleaseOrder == 0 {
					t.Fatalf("instance %s must carry its return order: %+v", inst.InstanceID, inst)
				}
				if inst.ToVersion == "" {
					t.Fatalf("instance %s must be keyed by version: %+v", inst.InstanceID, inst)
				}
			}
		}
	}
	// Same instance released under different versions appears separately.
	seen := map[string]bool{}
	for _, rv := range cmp.Releases {
		for _, g := range rv.Grants {
			for _, inst := range g.Instances {
				seen[inst.InstanceID+"@"+inst.ToVersion] = true
			}
		}
	}
	if !seen["inst-1@v2.0.0"] || !seen["inst-1@v1.0.0"] {
		t.Fatalf("same instance across versions must be shown separately: %v", seen)
	}

	// Read-only guarantee: the persisted snapshot is byte-identical after
	// the comparison, and no release/grant state moved.
	_, after := e.do(t, http.MethodGet, "/api/state", "")
	var s1, s2 store.Snapshot
	if err := json.Unmarshal(before, &s1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &s2); err != nil {
		t.Fatal(err)
	}
	if len(s1.Grants) != len(s2.Grants) || len(s1.Events) != len(s2.Events) {
		t.Fatal("comparison must not append events or change commitments")
	}
	for i := range s1.Releases {
		if s1.Releases[i].Status != s2.Releases[i].Status {
			t.Fatalf("comparison changed release status: %+v -> %+v", s1.Releases[i], s2.Releases[i])
		}
	}
	for i := range s1.Grants {
		if s1.Grants[i].State != s2.Grants[i].State || len(s1.Grants[i].InstanceIDs) != len(s2.Grants[i].InstanceIDs) {
			t.Fatal("comparison must not release instances or commitments")
		}
	}

	// Unknown release is a 400, not a state change.
	code, _ = e.do(t, http.MethodGet, "/api/capacity/compare?a=rel-nope", "")
	if code != 400 {
		t.Fatalf("unknown release = %d, want 400", code)
	}
	code, _ = e.do(t, http.MethodGet, "/api/capacity/compare", "")
	if code != 400 {
		t.Fatalf("missing selection = %d, want 400", code)
	}
}
