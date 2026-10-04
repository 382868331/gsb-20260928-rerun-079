package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// The cross-release capacity compare endpoint must be strictly read-only:
// filtering, viewing history and exporting never acquire commitments,
// release instances, or change release state.
func TestCapacityCompareReadOnly(t *testing.T) {
	e := newTestEnv(t)

	code, body := e.do(t, http.MethodPost, "/api/releases",
		`{"serviceId":"svc-order","targetVersion":"v2.0.0","batchSize":2,"strategy":"rolling","failure":{"mode":"none"}}`)
	if code != 200 {
		t.Fatalf("create release = %d %s", code, body)
	}
	var rel struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		t.Fatal(err)
	}

	// Run the release to completion.
	deadline := time.Now().Add(5 * time.Second)
	var final store.Snapshot
	done := false
	for time.Now().Before(deadline) && !done {
		_ = e.svc.Engine().Tick(time.Now())
		_, data := e.do(t, http.MethodGet, "/api/state", "")
		if err := json.Unmarshal(data, &final); err != nil {
			t.Fatal(err)
		}
		for _, r := range final.Releases {
			if r.ID == rel.ID && r.Status == "succeeded" {
				done = true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !done {
		t.Fatal("release never succeeded")
	}

	// Baseline state fingerprint before any compare calls.
	_, before := e.do(t, http.MethodGet, "/api/state", "")
	var snapBefore store.Snapshot
	if err := json.Unmarshal(before, &snapBefore); err != nil {
		t.Fatal(err)
	}

	// Unfiltered compare includes the finished release with its grants,
	// per-instance return order and zone policies.
	code, body = e.do(t, http.MethodGet, "/api/capacity/compare", "")
	if code != 200 {
		t.Fatalf("compare = %d %s", code, body)
	}
	var res engine.CapacityCompareResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Releases) != 1 || res.Releases[0].ReleaseID != rel.ID {
		t.Fatalf("expected the finished release in compare: %+v", res.Releases)
	}
	rc := res.Releases[0]
	if len(rc.Grants) == 0 {
		t.Fatal("expected grant windows for the finished release")
	}
	for _, g := range rc.Grants {
		if g.State != "released" || g.ReleasedAt == "" {
			t.Fatalf("grant should be released with a timestamp: %+v", g)
		}
	}
	if len(rc.Returns) == 0 {
		t.Fatal("expected per-instance return order")
	}
	for i, r := range rc.Returns {
		if r.Order != i+1 {
			t.Fatalf("return order must be sequential: %+v", rc.Returns)
		}
	}
	if len(rc.Occupancy) == 0 || len(rc.Policies) == 0 {
		t.Fatalf("expected occupancy and policies: %+v", rc)
	}

	// Filters: matching service + ended status keeps it, wrong ones drop it.
	check := func(query string, want int) {
		t.Helper()
		code, body := e.do(t, http.MethodGet, "/api/capacity/compare?"+query, "")
		if code != 200 {
			t.Fatalf("compare?%s = %d %s", query, code, body)
		}
		var r engine.CapacityCompareResult
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatal(err)
		}
		if len(r.Releases) != want {
			t.Fatalf("compare?%s returned %d releases, want %d", query, len(r.Releases), want)
		}
	}
	check("serviceId=svc-order", 1)
	check("serviceId=svc-pay", 0)
	check("status=ended", 1)
	check("status=succeeded", 1)
	check("status=cancelled", 0)
	check("from=2000-01-01", 1)
	check("from=2999-01-01", 0)
	check("to=2999-01-01", 1)
	check("zoneId=zone-a", 1)
	check("zoneId=zone-nope", 0)

	// Export/view must not have changed anything on disk.
	_, after := e.do(t, http.MethodGet, "/api/state", "")
	var snapAfter store.Snapshot
	if err := json.Unmarshal(after, &snapAfter); err != nil {
		t.Fatal(err)
	}
	if len(snapAfter.Grants) != len(snapBefore.Grants) ||
		len(snapAfter.Releases) != len(snapBefore.Releases) ||
		len(snapAfter.Events) != len(snapBefore.Events) ||
		len(snapAfter.Ops) != len(snapBefore.Ops) {
		t.Fatalf("compare endpoint mutated state: before %d grants/%d releases/%d events, after %d/%d/%d",
			len(snapBefore.Grants), len(snapBefore.Releases), len(snapBefore.Events),
			len(snapAfter.Grants), len(snapAfter.Releases), len(snapAfter.Events))
	}
	for i, g := range snapAfter.Grants {
		gb, _ := json.Marshal(g)
		pb, _ := json.Marshal(snapBefore.Grants[i])
		if string(gb) != string(pb) {
			t.Fatalf("grant changed by compare: %+v -> %+v", snapBefore.Grants[i], g)
		}
	}
	for i, r := range snapAfter.Releases {
		if r.Status != snapBefore.Releases[i].Status {
			t.Fatalf("release status changed by compare: %+v -> %+v", snapBefore.Releases[i], r)
		}
	}
}
