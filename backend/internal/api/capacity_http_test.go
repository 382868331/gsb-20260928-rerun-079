package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"releasectl/internal/store"
)

// End-to-end over HTTP: configure zone capacity, run two parallel releases
// that are individually safe but conflict on shared zone capacity, verify
// the waiting reason is queryable, then inject a zone fault and confirm the
// commitment is released by real instance state after rollback.
func TestZoneCapacityHTTPFlow(t *testing.T) {
	e := newTestEnv(t)

	// Zone capacity entry: zone-a shared capacity fits one instance only.
	code, body := e.do(t, http.MethodPut, "/api/zones/zone-a/capacity",
		`{"minAvailable":{"svc-order":1},"sharedCapacity":1}`)
	if code != 202 {
		t.Fatalf("set capacity = %d %s", code, body)
	}

	// Compare plans for the order service.
	code, body = e.do(t, http.MethodPost, "/api/plans/compare",
		`{"serviceId":"svc-order","batchSize":2}`)
	if code != 200 {
		t.Fatalf("compare = %d %s", code, body)
	}
	var cmp struct {
		Plans []struct {
			Strategy string `json:"strategy"`
			Batches  []struct {
				InstanceIDs []string `json:"instanceIds"`
				ZoneNames   []string `json:"zoneNames"`
				CapacityOK  bool     `json:"capacityOk"`
			} `json:"batches"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(body, &cmp); err != nil {
		t.Fatal(err)
	}
	if len(cmp.Plans) != 2 || len(cmp.Plans[0].Batches) == 0 {
		t.Fatalf("compare plans malformed: %+v", cmp)
	}

	// First release (order) takes the zone-a commitment.
	code, body = e.do(t, http.MethodPost, "/api/releases",
		`{"serviceId":"svc-order","targetVersion":"v2.0.0","batchSize":2,"strategy":"rolling","failure":{"mode":"none"}}`)
	if code != 200 {
		t.Fatalf("create rel1 = %d %s", code, body)
	}
	var rel1 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rel1); err != nil {
		t.Fatal(err)
	}

	// Let rel1's first batch move instances, then add a pay instance and a
	// second release that also needs zone-a.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = e.svc.Engine().Tick(time.Now())
		_, data := e.do(t, http.MethodGet, "/api/state", "")
		var s store.Snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		moved := 0
		for _, inst := range s.Instances {
			if inst.CurrentVersion == "v2.0.0" {
				moved++
			}
		}
		if moved >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code, body = e.do(t, http.MethodPost, "/api/services/svc-pay/instances", ""); code != 200 {
		t.Fatalf("add pay instance = %d %s", code, body)
	}
	code, body = e.do(t, http.MethodPost, "/api/releases",
		`{"serviceId":"svc-pay","targetVersion":"v2.0.0","batchSize":1,"strategy":"rolling","failure":{"mode":"none"}}`)
	if code != 200 {
		t.Fatalf("create rel2 = %d %s", code, body)
	}

	// rel2 must wait with a queryable reason naming rel1.
	var waitSnap store.Snapshot
	found := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !found {
		_ = e.svc.Engine().Tick(time.Now())
		_, data := e.do(t, http.MethodGet, "/api/state", "")
		if err := json.Unmarshal(data, &waitSnap); err != nil {
			t.Fatal(err)
		}
		for _, b := range waitSnap.Batches {
			if b.ReleaseID != rel1.ID && b.CapacityWait != "" {
				found = true
				if !strings.Contains(b.CapacityWait, rel1.ID) {
					t.Fatalf("wait reason must name the holder %s: %s", rel1.ID, b.CapacityWait)
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !found {
		t.Fatal("second release never showed a capacity wait reason")
	}
	held := 0
	for _, g := range waitSnap.Grants {
		if g.State == "held" {
			held++
		}
	}
	if held == 0 {
		t.Fatal("no queryable held commitment while releases compete")
	}

	// Zone fault: inject into zone-a; the in-flight batch must roll back and
	// every commitment must eventually be released by real instance state.
	code, _ = e.do(t, http.MethodPost, "/api/zones/zone-a/failure", `{"failed":true}`)
	if code != 202 {
		t.Fatalf("zone failure = %d", code)
	}
	deadline = time.Now().Add(5 * time.Second)
	allReleased := false
	var final store.Snapshot
	for time.Now().Before(deadline) && !allReleased {
		_ = e.svc.Engine().Tick(time.Now())
		_, data := e.do(t, http.MethodGet, "/api/state", "")
		if err := json.Unmarshal(data, &final); err != nil {
			t.Fatal(err)
		}
		allReleased = len(final.Grants) > 0
		for _, g := range final.Grants {
			if g.State == "held" {
				allReleased = false
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !allReleased {
		t.Fatalf("commitments still held after zone-failure rollback: %+v", final.Grants)
	}
	for _, g := range final.Grants {
		if g.ReleaseWhy == "" || g.ReleasedAt == "" {
			t.Fatalf("released grant must carry its evidence: %+v", g)
		}
	}
}
