package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

func grantsOf(st *store.Store) []store.CapacityGrant {
	var out []store.CapacityGrant
	st.View(func(tx *store.Tx) {
		out = append(out, tx.Grants()...)
	})
	return out
}

func batchesOf(st *store.Store) []store.Batch {
	var out []store.Batch
	st.View(func(tx *store.Tx) {
		out = append(out, tx.Batches()...)
	})
	return out
}

func releasesOf(st *store.Store) []store.Release {
	var out []store.Release
	st.View(func(tx *store.Tx) {
		out = append(out, tx.Releases()...)
	})
	return out
}

func allInstances(st *store.Store) []store.Instance {
	var out []store.Instance
	st.View(func(tx *store.Tx) {
		out = append(out, tx.Instances()...)
	})
	return out
}

// Two individually safe plans run in parallel: the shared zone capacity
// only fits one of them. The first release gets a queryable commitment; the
// second waits with a persisted, reopenable reason and proceeds only after
// the first release frees its slots by real instance state.
func TestParallelReleasesCompeteForSharedZoneCapacity(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0

	// zone-a shared capacity fits exactly one instance at a time.
	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	// A second service with an instance in zone-a (and one in zone-b).
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}

	rel1, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "rolling",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Let rel1's first batch actually move instances onto v2.0.0 so the
	// pay->order update_first rule sees a migrating (mixed) dependent and
	// does not compat-block rel2 before capacity is even evaluated.
	runUntil(t, svc, &now, 150*time.Millisecond)
	rel2, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v2.0.0", BatchSize: 1, Strategy: "rolling",
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	runUntil(t, svc, &now, 300*time.Millisecond)
	st := snapshotStore(t, dir)

	// rel1 batch 1 holds commitments in both zones; rel2 batch 1 (a zone-a
	// instance) must wait, and the reason must name rel1.
	var wait string
	for _, b := range batchesOf(st) {
		if b.ReleaseID == rel2.ID && b.Index == 0 {
			wait = b.CapacityWait
		}
	}
	if wait == "" {
		t.Fatal("rel2 batch 1 should be waiting for zone capacity")
	}
	if !strings.Contains(wait, "共享容量") || !strings.Contains(wait, rel1.ID) {
		t.Fatalf("wait reason should name shared capacity and holder %s: %s", rel1.ID, wait)
	}
	held := 0
	for _, g := range grantsOf(st) {
		if g.State == "held" {
			held++
			if g.ReleaseID != rel1.ID {
				t.Fatalf("held grant belongs to %s, want %s", g.ReleaseID, rel1.ID)
			}
		}
	}
	if held == 0 {
		t.Fatal("rel1 should hold queryable capacity commitments")
	}

	// The waiting state survives a reopen (process restart).
	svc2 := newService(t, dir)
	st2 := snapshotStore(t, dir)
	stillWaiting := false
	for _, b := range batchesOf(st2) {
		if b.ReleaseID == rel2.ID && b.Index == 0 && b.CapacityWait != "" {
			stillWaiting = true
		}
	}
	if !stillWaiting {
		t.Fatal("capacity wait reason must survive a reopen")
	}

	// Let rel1 finish batch 1 (updates + health). Its slots free up by real
	// instance state, then rel2 may proceed.
	runUntil(t, svc2, &now, 2*time.Second)
	st3 := snapshotStore(t, dir)
	released := 0
	for _, g := range grantsOf(st3) {
		if g.State == "released" {
			released++
			if g.ReleaseWhy == "" || g.ReleasedAt == "" {
				t.Fatalf("released grant must record when and why: %+v", g)
			}
		}
	}
	if released == 0 {
		t.Fatal("grants must be released once instances are verified healthy")
	}
	var rel2b0 *store.Batch
	for i, b := range batchesOf(st3) {
		if b.ReleaseID == rel2.ID && b.Index == 0 {
			bb := batchesOf(st3)[i]
			rel2b0 = &bb
		}
	}
	if rel2b0 == nil || (rel2b0.Phase == "pending" && rel2b0.CapacityWait != "") {
		t.Fatalf("rel2 batch 1 still waiting after capacity freed: %+v", rel2b0)
	}
}

// A failed batch rolls back; the capacity commitment is released only when
// every instance is verified back on its original version.
func TestRollbackReleasesCapacityByRealState(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if err := svc.SetZoneCapacity("zone-a", map[string]int{"svc-order": 2}, 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "rolling",
		Failure: store.FailureSpec{Mode: "half_batch"},
	}, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)
	st := snapshotStore(t, dir)
	for _, g := range grantsOf(st) {
		if g.State != "released" {
			t.Fatalf("grant %s still held after rollback: %+v", g.ID, g)
		}
	}
	for _, inst := range allInstances(st) {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s on %s, want v1.0.0", inst.ID, inst.CurrentVersion)
		}
	}
	rels := releasesOf(st)
	if rels[0].Status != "rolled_back" {
		t.Fatalf("release status = %s, want rolled_back", rels[0].Status)
	}
}

// A zone fault fails health observation inside the zone; the batch rolls
// back and its commitment is released by real instance state.
func TestZoneFailureDrivesRollbackAndRelease(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if err := svc.SetZoneCapacity("zone-a", map[string]int{"svc-order": 2}, 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "rolling",
	}, now); err != nil {
		t.Fatal(err)
	}
	// Let batch 1 update, then inject the zone fault before health resolves.
	runUntil(t, svc, &now, 150*time.Millisecond)
	if err := svc.SetZoneFailure("zone-a", true, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)
	st := snapshotStore(t, dir)
	rels := releasesOf(st)
	if rels[0].Status != "rolled_back" {
		t.Fatalf("release status = %s, want rolled_back", rels[0].Status)
	}
	for _, g := range grantsOf(st) {
		if g.State != "released" {
			t.Fatalf("grant %s still held after zone-failure rollback", g.ID)
		}
	}
	for _, inst := range allInstances(st) {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s on %s after zone-failure rollback", inst.ID, inst.CurrentVersion)
		}
	}
}

// Cancelling the waiting release must not touch the other release's
// completed batches or its already released commitments.
func TestCancelWaitingReleaseKeepsOtherBatches(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	rel1, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "rolling",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Let rel1's first batch move instances so rel2 is not compat-blocked.
	runUntil(t, svc, &now, 150*time.Millisecond)
	rel2, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v2.0.0", BatchSize: 1, Strategy: "rolling",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// rel1 keeps zone-a occupied batch after batch, so rel2 is still
	// waiting when the operator cancels it.
	runUntil(t, svc, &now, 400*time.Millisecond)
	waiting := false
	for _, b := range batchesOf(snapshotStore(t, dir)) {
		if b.ReleaseID == rel2.ID && b.Index == 0 && b.CapacityWait != "" {
			waiting = true
		}
	}
	if !waiting {
		t.Fatal("rel2 should still be waiting for capacity before cancel")
	}
	if err := svc.CancelRelease(rel2.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)
	st := snapshotStore(t, dir)
	var r1b0Phase string
	for _, b := range batchesOf(st) {
		if b.ReleaseID == rel1.ID && b.Index == 0 {
			r1b0Phase = b.Phase
		}
	}
	if r1b0Phase != "succeeded" {
		t.Fatalf("rel1 batch 1 must stay succeeded, got %s", r1b0Phase)
	}
	for _, r := range releasesOf(st) {
		if r.ID == rel2.ID && r.Status != "cancelled" {
			t.Fatalf("rel2 status = %s, want cancelled", r.Status)
		}
	}
	for _, g := range grantsOf(st) {
		if g.State == "held" {
			t.Fatalf("no grant may stay held after both releases ended: %+v", g)
		}
	}
}

// ComparePlans returns both candidate strategies with per-batch zone
// footprints and capacity verdicts.
func TestComparePlans(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	if err := svc.SetZoneCapacity("zone-a", map[string]int{"svc-order": 2}, 0, t0); err != nil {
		t.Fatal(err)
	}
	// A third zone with two more instances makes the strategies differ:
	// sequential batching packs same-zone instances together, spreading
	// does not.
	if _, err := svc.CreateZone("zone-c", "故障区域 C", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-order", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-order", t0); err != nil {
		t.Fatal(err)
	}
	plans, err := svc.ComparePlans("svc-order", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	total := 0
	for _, p := range plans {
		if len(p.Batches) != 3 {
			t.Fatalf("%s: batches = %d, want 3", p.Strategy, len(p.Batches))
		}
		for _, b := range p.Batches {
			total += len(b.InstanceIDs)
			if len(b.ZoneNames) == 0 {
				t.Fatalf("%s batch %d has no zone footprint", p.Strategy, b.Index)
			}
		}
	}
	if total != 16 {
		t.Fatalf("both plans must cover all 8 instances each, got %d", total)
	}
	// zone-a has 3 order instances and min available 2: the sequential plan
	// packs two zone-a instances into batch 1 (inst-1, inst-3), which would
	// break the lower bound, while the spread plan takes one per zone.
	var rolling, spread engine.Plan
	for _, p := range plans {
		if p.Strategy == "rolling" {
			rolling = p
		} else {
			spread = p
		}
	}
	if rolling.Batches[0].CapacityOK {
		t.Fatal("rolling plan batch 1 should violate zone-a minimum availability")
	}
	for _, b := range spread.Batches {
		if !b.CapacityOK {
			t.Fatalf("spread plan batch %d should be capacity-safe: %s", b.Index, b.Reason)
		}
	}
}
