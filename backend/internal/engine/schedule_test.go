package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// heldPerZone counts instances covered by held grants per zone, and also
// detects an instance covered by more than one held grant (double
// allocation).
func heldPerZone(grants []store.CapacityGrant) (map[string]int, map[string]int) {
	perZone := map[string]int{}
	perInst := map[string]int{}
	for _, g := range grants {
		if g.State != "held" {
			continue
		}
		perZone[g.ZoneID] += len(g.InstanceIDs)
		for _, id := range g.InstanceIDs {
			perInst[id]++
		}
	}
	return perZone, perInst
}

func eventsOf(st *store.Store) []store.Event {
	var out []store.Event
	st.View(func(tx *store.Tx) {
		out = append(out, tx.Events()...)
	})
	return out
}

// Interleaved drill: two services compete for one shared slot in zone-a,
// the high-priority release waits then jumps the queue, its health failure
// rolls back while still holding real capacity, zone-b is shrunk mid-run,
// and every tick is checked for oversell and double release.
func TestCoordinatedContentionShrinkAndRollback(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0

	if _, err := svc.CreateVersion("v1.5.0", "支付/库存服务 1.5.0", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil { // zone-a
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil { // zone-b
		t.Fatal(err)
	}
	// zone-a fits exactly one occupied instance across all services;
	// zone-b starts with room for two.
	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetZoneCapacity("zone-b", map[string]int{}, 2, now); err != nil {
		t.Fatal(err)
	}

	// Low-priority release goes first and occupies the zone-a slot.
	relLow, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 1,
		Strategy: "rolling", Priority: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 75*time.Millisecond) // batch 1 updating, grant held

	// High-priority release arrives while the slot is occupied. Its zone-a
	// instance will fail health observation and roll back.
	payZoneA := ""
	for _, inst := range allInstances(snapshotStore(t, dir)) {
		if inst.ServiceID == "svc-pay" && inst.ZoneID == "zone-a" {
			payZoneA = inst.ID
		}
	}
	if payZoneA == "" {
		t.Fatal("expected a svc-pay instance in zone-a")
	}
	_ = payZoneA // the zone-a member fails health via half_batch below
	relHigh, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1,
		Strategy: "rolling", Priority: 9,
		Failure: store.FailureSpec{Mode: "half_batch"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Invariant checker: no oversell, no double allocation, grants are
	// released at most once and never resurrected.
	grantState := map[string]string{}
	checkInvariants := func() {
		st := snapshotStore(t, dir)
		grants := grantsOf(st)
		perZone, perInst := heldPerZone(grants)
		if perZone["zone-a"] > 1 {
			t.Fatalf("oversell: zone-a shared capacity 1 but %d instances held", perZone["zone-a"])
		}
		for id, n := range perInst {
			if n > 1 {
				t.Fatalf("instance %s covered by %d held grants", id, n)
			}
		}
		for _, g := range grants {
			prev, seen := grantState[g.ID]
			if seen && prev == "released" && g.State != "released" {
				t.Fatalf("grant %s released then became %s again (double release)", g.ID, g.State)
			}
			if seen && prev == "released" && g.State == "released" {
				if g.ReleasedAt == "" || g.ReleaseWhy == "" {
					t.Fatalf("released grant %s missing release evidence", g.ID)
				}
			}
			grantState[g.ID] = g.State
		}
	}
	runChecked := func(d time.Duration) {
		deadline := now.Add(d)
		for now.Before(deadline) {
			now = now.Add(25 * time.Millisecond)
			if err := svc.Engine().Tick(now); err != nil {
				t.Fatal(err)
			}
			checkInvariants()
		}
	}

	// While rel-low's batch is in flight, rel-high must wait: in-flight
	// instances are never preempted.
	runChecked(50 * time.Millisecond)
	st := snapshotStore(t, dir)
	var highWait string
	for _, b := range batchesOf(st) {
		if b.ReleaseID == relHigh.ID && b.Index == 0 {
			highWait = b.CapacityWait
		}
	}
	if highWait == "" || !strings.Contains(highWait, relLow.ID) {
		t.Fatalf("high-priority release should wait for the in-flight batch, naming %s: %q", relLow.ID, highWait)
	}

	// rel-low batch 1 finishes; the freed slot must go to the waiting
	// high-priority release, not to rel-low's next zone-a batch.
	highStarted := false
	for i := 0; i < 80 && !highStarted; i++ {
		runChecked(25 * time.Millisecond)
		for _, b := range batchesOf(snapshotStore(t, dir)) {
			if b.ReleaseID == relHigh.ID && b.Index == 0 && b.Phase != "pending" {
				highStarted = true
			}
		}
	}
	if !highStarted {
		t.Fatal("high-priority release should acquire the freed slot first")
	}
	for _, b := range batchesOf(snapshotStore(t, dir)) {
		if b.ReleaseID == relLow.ID && b.Index == 2 && b.Phase != "pending" {
			t.Fatal("low-priority release jumped ahead of the waiting high-priority release")
		}
	}

	// Mid-run zone-b shrink: only later batches are re-evaluated; held
	// commitments stay valid and a reorder reason is persisted.
	heldBefore, _ := heldPerZone(grantsOf(snapshotStore(t, dir)))
	if err := svc.SetZoneCapacity("zone-b", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	heldAfter, _ := heldPerZone(grantsOf(snapshotStore(t, dir)))
	if heldAfter["zone-b"] != heldBefore["zone-b"] {
		t.Fatalf("zone shrink must not touch held commitments: %d -> %d", heldBefore["zone-b"], heldAfter["zone-b"])
	}
	foundReorder := false
	for _, e := range eventsOf(snapshotStore(t, dir)) {
		if strings.Contains(e.Message, "重排评估") && strings.Contains(e.Message, "区域降容") {
			foundReorder = true
		}
	}
	if !foundReorder {
		t.Fatal("zone shrink must persist a reorder-assessment reason")
	}

	// rel-high's health failure drives a rollback; while rolling back its
	// instances still occupy real capacity (no planned-return fakery), then
	// the grant is released exactly once by real instance state.
	runChecked(2 * time.Second)
	st = snapshotStore(t, dir)
	var highRel store.Release
	for _, r := range releasesOf(st) {
		if r.ID == relHigh.ID {
			highRel = r
		}
	}
	if highRel.Status != "rolled_back" {
		t.Fatalf("high-priority release status = %s, want rolled_back", highRel.Status)
	}
	for _, inst := range allInstances(st) {
		if inst.ID == payZoneA && inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("rolled-back instance on %s, want v1.0.0", inst.CurrentVersion)
		}
	}
	releasedHigh := 0
	for _, g := range grantsOf(st) {
		if g.ReleaseID == relHigh.ID && g.State == "released" {
			releasedHigh++
		}
		if g.ReleaseID == relHigh.ID && g.State == "held" {
			t.Fatalf("grant %s still held after rollback completed", g.ID)
		}
	}
	if releasedHigh == 0 {
		t.Fatal("rollback must release the capacity commitment by real instance state")
	}

	// The low-priority release is not starved: it finishes after the
	// high-priority release's rollback frees the slot.
	runChecked(6 * time.Second)
	st = snapshotStore(t, dir)
	for _, r := range releasesOf(st) {
		if r.ID == relLow.ID && r.Status != "succeeded" {
			t.Fatalf("low-priority release status = %s, want succeeded", r.Status)
		}
	}
	for _, inst := range allInstances(st) {
		if inst.ServiceID == "svc-order" && inst.CurrentVersion != "v2.0.0" {
			t.Fatalf("instance %s on %s, want v2.0.0", inst.ID, inst.CurrentVersion)
		}
	}
}

// The coordination plan gives an executable batch order and names the
// concrete capacity conflict when a deadline cannot be met.
func TestScheduleExplainsDeadlineConflict(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.CreateVersion("v1.5.0", "支付服务 1.5.0", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	deadline := now.Add(900 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	relA, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 1,
		Strategy: "rolling", Priority: 5, Deadline: deadline,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1,
		Strategy: "rolling", Priority: 9,
	}, now); err != nil {
		t.Fatal(err)
	}

	view := svc.ScheduleView(now)
	if len(view.Queue) == 0 {
		t.Fatal("schedule must list the executable batch order")
	}
	// Queue is grouped by coordinated priority order and every entry names
	// its release and batch.
	for _, item := range view.Queue {
		if item.ReleaseID == "" || item.State == "" {
			t.Fatalf("queue item missing release/state: %+v", item)
		}
	}
	var estA *store.ReleaseEstimate
	for i := range view.Estimates {
		if view.Estimates[i].ReleaseID == relA.ID {
			estA = &view.Estimates[i]
		}
	}
	if estA == nil {
		t.Fatal("schedule must estimate every in-flight release")
	}
	if estA.OnTime {
		t.Fatalf("deadline %s is unreachable for 6 batches; estimate must say so", deadline)
	}
	if len(estA.Conflicts) == 0 {
		t.Fatal("estimate must name the concrete capacity conflict")
	}
	joined := strings.Join(estA.Conflicts, "；")
	if !strings.Contains(joined, "故障区域 A") {
		t.Fatalf("conflict must name the zone: %s", joined)
	}
}

// A long-waiting low-priority release ages past a freshly created
// higher-priority one, so it cannot starve.
func TestWaitingReleaseAgesOutOfStarvation(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.CreateVersion("v1.5.0", "支付/库存服务 1.5.0", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil { // zone-a
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-inventory", now); err != nil { // zone-a
		t.Fatal(err)
	}
	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	invZoneA := ""
	for _, inst := range allInstances(snapshotStore(t, dir)) {
		if inst.ServiceID == "svc-inventory" && inst.ZoneID == "zone-a" {
			invZoneA = inst.ID
		}
	}
	if invZoneA == "" {
		t.Fatal("expected a svc-inventory instance in zone-a")
	}

	// Blocker holds the only zone-a slot and stays paused on a health
	// failure, so the slot stays occupied by real (unsettled) state.
	blocker, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-inventory", TargetVersion: "v1.5.0", BatchSize: 1,
		Strategy: "rolling", Priority: 0,
		Failure: store.FailureSpec{Mode: "half_batch", PauseOnFailure: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 300*time.Millisecond)

	// Low-priority release queues behind the blocker.
	relLow, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 1,
		Strategy: "rolling", Priority: 0,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 100*time.Millisecond)
	st := snapshotStore(t, dir)
	waiting := false
	for _, b := range batchesOf(st) {
		if b.ReleaseID == relLow.ID && b.Index == 0 && b.CapacityWait != "" {
			waiting = true
		}
	}
	if !waiting {
		t.Fatal("low-priority release should be waiting for the occupied slot")
	}

	// 30 minutes of waiting go by; the blocker stays paused.
	for i := 0; i < 30; i++ {
		now = now.Add(time.Minute)
		if err := svc.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
	}
	// A fresher, nominally higher-priority release arrives.
	relNew, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1,
		Strategy: "rolling", Priority: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// The aged low-priority release outranks the fresh one in the plan.
	view := svc.ScheduleView(now)
	pos := map[string]int{}
	for _, item := range view.Queue {
		if item.State == "running" {
			continue
		}
		if _, seen := pos[item.ReleaseID]; !seen {
			pos[item.ReleaseID] = item.Order
		}
	}
	if pos[relLow.ID] > pos[relNew.ID] {
		t.Fatalf("aged low-priority release should be scheduled before the fresh one: low@%d new@%d",
			pos[relLow.ID], pos[relNew.ID])
	}

	// Unblock: rollback the blocker. The freed slot goes to the aged
	// low-priority release, not the fresh higher-priority one.
	if err := svc.ReviewRollback(blocker.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 2*time.Second)
	st = snapshotStore(t, dir)
	lowAcquired, newAcquired := false, false
	for _, g := range grantsOf(st) {
		if g.ReleaseID == relLow.ID {
			lowAcquired = true
		}
		if g.ReleaseID == relNew.ID && g.State == "held" {
			newAcquired = true
		}
	}
	if !lowAcquired {
		t.Fatal("aged low-priority release should acquire the freed slot")
	}
	if newAcquired {
		t.Fatal("fresh release must not starve the long-waiting one")
	}
}
