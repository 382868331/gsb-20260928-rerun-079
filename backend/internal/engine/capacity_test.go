package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

func setPolicy(t *testing.T, svc *engine.Service, zone string, min map[string]int, shared int, now time.Time) {
	t.Helper()
	if _, err := svc.SetCapacityPolicy(zone, min, shared, now); err != nil {
		t.Fatal(err)
	}
}

// Two plans that are each safe alone cannot both hold the zone shared pool:
// the first gets a queryable commitment, the second waits with a persisted
// reason, and proceeds only after the first commitment is released.
func TestCapacityContentionOneWaits(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, svc, "zone-a", map[string]int{}, 1, now)

	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v2.0.0", BatchSize: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	runUntil(t, svc, &now, 300*time.Millisecond)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		held := 0
		for _, r := range tx.Reservations() {
			if r.State == "held" && r.ZoneID == "zone-a" {
				held++
				if len(r.Basis) == 0 {
					t.Fatal("held reservation must record its per-instance basis")
				}
			}
		}
		if held != 1 {
			t.Fatalf("exactly one zone-a commitment may be held, got %d", held)
		}
		var payBatch *store.Batch
		for i := range tx.Batches() {
			if tx.Batches()[i].ReleaseID == payRel.ID && tx.Batches()[i].Index == 0 {
				payBatch = &tx.Batches()[i]
			}
		}
		if payBatch == nil || payBatch.Phase != "pending" || payBatch.CapacityWaiting == "" {
			t.Fatalf("pay batch must wait with a reason, got %+v", payBatch)
		}
		if !strings.Contains(payBatch.CapacityWaiting, "共享容量") {
			t.Fatalf("waiting reason must name the shared pool, got %q", payBatch.CapacityWaiting)
		}
	})

	// Once the order release finishes, its commitments are released and the
	// waiting plan proceeds to completion.
	runUntil(t, svc, &now, 8*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.Status != "succeeded" {
				t.Fatalf("release %s = %s, want succeeded", r.ID, r.Status)
			}
		}
		for _, res := range tx.Reservations() {
			if res.State != "released" {
				t.Fatalf("reservation %s still held after success", res.ID)
			}
			if len(res.ReleaseBasis) == 0 || res.ReleaseReason == "" {
				t.Fatalf("released reservation %s lacks release evidence", res.ID)
			}
		}
	})
	_ = orderRel
}

// A rollback frees the commitment only from the real member states: while
// instances are mid-rollback the commitment stays held; once every instance
// is verified back on the baseline it is released with that evidence.
func TestCapacityReleasedByRealRollback(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	setPolicy(t, svc, "zone-a", map[string]int{"svc-order": 1}, 0, now)

	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "half_batch"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 10*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == rel.ID && r.Status != "rolled_back" {
				t.Fatalf("release = %s, want rolled_back", r.Status)
			}
		}
		var res *store.CapacityReservation
		for i := range tx.Reservations() {
			if tx.Reservations()[i].ReleaseID == rel.ID {
				res = &tx.Reservations()[i]
			}
		}
		if res == nil {
			t.Fatal("expected a capacity commitment for the batch")
		}
		if res.State != "released" || !strings.Contains(res.ReleaseReason, "回退") {
			t.Fatalf("commitment must be released by the rollback, got %s %q", res.State, res.ReleaseReason)
		}
		joined := strings.Join(res.ReleaseBasis, ";")
		if !strings.Contains(joined, "v1.0.0") {
			t.Fatalf("release basis must show instances really back on baseline, got %q", joined)
		}
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion != "v1.0.0" {
				t.Fatalf("instance %s = %s, want baseline", inst.ID, inst.CurrentVersion)
			}
		}
	})
}

// Cancelling one waiting plan must not touch the other plan's completed
// batches, and its pending batches never hold capacity.
func TestCancelWaitingPlanKeepsOther(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, svc, "zone-a", map[string]int{}, 1, now)

	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v2.0.0", BatchSize: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 300*time.Millisecond)
	if err := svc.CancelRelease(payRel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 8*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		succeededOrderBatches := 0
		for _, b := range tx.Batches() {
			if b.ReleaseID == orderRel.ID && b.Phase == "succeeded" {
				succeededOrderBatches++
			}
			if b.ReleaseID == payRel.ID && b.Phase == "pending" {
				t.Fatalf("cancelled plan must not keep pending batches: %s", b.ID)
			}
		}
		if succeededOrderBatches != 6 {
			t.Fatalf("order completed batches = %d, want 6 (cancel of the other plan must not interfere)", succeededOrderBatches)
		}
		for _, res := range tx.Reservations() {
			if res.State == "held" {
				t.Fatalf("no commitment may stay held at the end: %s", res.ID)
			}
		}
	})
}

// zone_spread batches each zone separately and interleaves them, so every
// batch touches exactly one fault zone.
func TestZoneSpreadBatchesStayInOneZone(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "zone_spread",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		count := 0
		for _, b := range tx.Batches() {
			if b.ReleaseID != rel.ID {
				continue
			}
			count++
			zones := map[string]bool{}
			for _, m := range b.Members {
				for _, inst := range tx.Instances() {
					if inst.ID == m.InstanceID {
						zones[inst.ZoneID] = true
					}
				}
			}
			if len(zones) != 1 {
				t.Fatalf("zone_spread batch %s spans zones %v", b.ID, zones)
			}
		}
		if count != 4 {
			t.Fatalf("zone_spread with size 2 over 2 zones of 3 must make 4 batches, got %d", count)
		}
	})
}
