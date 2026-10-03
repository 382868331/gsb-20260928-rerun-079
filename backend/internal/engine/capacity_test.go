package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// slowHealthCfg makes health observation long enough that a batch can be
// paused while it holds freshly updated instances.
var slowHealthCfg = engine.Config{UpdateDelay: 100 * time.Millisecond, HealthDelay: 1000 * time.Millisecond, RollbackDelay: 100 * time.Millisecond}

func newServiceCfg(t *testing.T, dir string, cfg engine.Config) *engine.Service {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, cfg)
	if err := svc.Seed(t0); err != nil {
		t.Fatal(err)
	}
	return svc
}

func addInstances(t *testing.T, svc *engine.Service, serviceID string, n int, now time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := svc.AddInstance(serviceID, now); err != nil {
			t.Fatal(err)
		}
	}
}

func capacityOf(t *testing.T, dir string) store.CapacityView {
	t.Helper()
	var view store.CapacityView
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		view = engine.BuildCapacityView(tx)
	})
	return view
}

func poolRow(t *testing.T, view store.CapacityView, name string) store.PoolCapacity {
	t.Helper()
	for _, p := range view.Pools {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("pool %q not found in capacity view", name)
	return store.PoolCapacity{}
}

func serviceRow(t *testing.T, view store.CapacityView, serviceID, zoneID string) store.ZoneServiceCapacity {
	t.Helper()
	for _, row := range view.Services {
		if row.ServiceID == serviceID && row.ZoneID == zoneID {
			return row
		}
	}
	t.Fatalf("service %s zone %s not found in capacity view", serviceID, zoneID)
	return store.ZoneServiceCapacity{}
}

func releaseByID(t *testing.T, dir, id string) store.Release {
	t.Helper()
	var rel store.Release
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == id {
				rel = r
			}
		}
	})
	if rel.ID == "" {
		t.Fatalf("release %s not found", id)
	}
	return rel
}

// Two individually safe batches advancing at the same time must never drop
// the shared pool below its floor: the second one waits for the first one's
// commitment to be released.
func TestConcurrentBatchesRespectSharedFloor(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	addInstances(t, svc, "svc-pay", 4, now) // 2 east + 2 west
	if _, err := svc.CreateVersion("v1.5.0", "支付服务 1.5.0", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddPool(store.SharedPool{
		Name: "订单支付共享池", ZoneID: "zone-east",
		ServiceIDs: []string{"svc-order", "svc-pay"}, MinAvailable: 3,
	}, now); err != nil {
		t.Fatal(err)
	}

	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	sawPayBlocked := false
	deadline := now.Add(8 * time.Second)
	for now.Before(deadline) {
		now = now.Add(25 * time.Millisecond)
		if err := svc.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
		// The floor must hold after every single tick, not just at the end.
		row := poolRow(t, capacityOf(t, dir), "订单支付共享池")
		if row.Available < 3 {
			t.Fatalf("shared pool dropped below floor: available=%d (min 3)", row.Available)
		}
		if r := releaseByID(t, dir, payRel.ID); r.CapacityBlocked {
			sawPayBlocked = true
			if !strings.Contains(r.CapacityNote, "共享") {
				t.Fatalf("capacity note must explain the shared pool, got %q", r.CapacityNote)
			}
		}
	}
	if !sawPayBlocked {
		t.Fatal("expected the payment release to wait for capacity while the order batch held commitments")
	}
	if r := releaseByID(t, dir, orderRel.ID); r.Status != "succeeded" {
		t.Fatalf("order release = %s, want succeeded", r.Status)
	}
	if r := releaseByID(t, dir, payRel.ID); r.Status != "succeeded" {
		t.Fatalf("pay release = %s, want succeeded (it must resume once capacity frees)", r.Status)
	}
}

// A paused batch keeps its capacity commitment; a rolling-back instance
// stays occupied until the rollback op actually recovers it. Only then is
// the commitment released.
func TestCommitmentLifecycleAcrossPauseAndRollback(t *testing.T) {
	dir := t.TempDir()
	svc := newServiceCfg(t, dir, slowHealthCfg)
	now := t0
	if _, err := svc.AddPolicy(store.CapacityPolicy{
		ServiceID: "svc-order", ZoneID: "zone-east", MinAvailable: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 1,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 300*time.Millisecond) // batch 1 updated, observing
	if err := svc.PauseForReview(rel.ID, now); err != nil {
		t.Fatal(err)
	}

	row := serviceRow(t, capacityOf(t, dir), "svc-order", "zone-east")
	if row.Occupied != 1 || row.Available != 2 {
		t.Fatalf("paused batch must hold its commitment: occupied=%d available=%d", row.Occupied, row.Available)
	}
	view := capacityOf(t, dir)
	if len(view.Commitments) != 1 || view.Commitments[0].State != "paused" {
		t.Fatalf("expected one paused commitment, got %+v", view.Commitments)
	}
	if view.Commitments[0].InstanceIDs[0] != "inst-1" {
		t.Fatalf("commitment must name the held instance, got %+v", view.Commitments[0].InstanceIDs)
	}

	if err := svc.ReviewRollback(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	// Rollback op is scheduled but has not landed: the instance is still
	// occupied. Flipping the plan to "rolling_back" must not free it.
	runUntil(t, svc, &now, 50*time.Millisecond)
	row = serviceRow(t, capacityOf(t, dir), "svc-order", "zone-east")
	if row.Occupied != 1 {
		t.Fatalf("instance mid-rollback must stay occupied, occupied=%d", row.Occupied)
	}

	runUntil(t, svc, &now, 2*time.Second)
	row = serviceRow(t, capacityOf(t, dir), "svc-order", "zone-east")
	if row.Occupied != 0 || row.Available != 3 {
		t.Fatalf("commitment must be released after recovery: occupied=%d available=%d", row.Occupied, row.Available)
	}
	if r := releaseByID(t, dir, rel.ID); r.Status != "rolled_back" {
		t.Fatalf("release = %s, want rolled_back", r.Status)
	}
	if view := capacityOf(t, dir); len(view.Commitments) != 0 {
		t.Fatalf("no commitments may remain, got %+v", view.Commitments)
	}
}

// A failed zone invalidates its capacity: admission blocks until the zone
// recovers, and an in-flight batch observing there rolls back.
func TestZoneFailureBlocksAdmissionUntilRecovery(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.AddPolicy(store.CapacityPolicy{
		ServiceID: "svc-order", ZoneID: "zone-east", MinAvailable: 1,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.FailZone("zone-east", now); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 500*time.Millisecond)
	r := releaseByID(t, dir, rel.ID)
	if !r.CapacityBlocked {
		t.Fatalf("release must wait for capacity while the zone is failed: %+v", r)
	}
	row := serviceRow(t, capacityOf(t, dir), "svc-order", "zone-east")
	if row.Available != 0 || !row.ZoneFailed {
		t.Fatalf("failed zone must report zero available capacity, got %+v", row)
	}
	for _, b := range batchesOf(t, dir, rel.ID) {
		if b.Phase != "pending" {
			t.Fatalf("no batch may expand into a failed zone, got %s", b.Phase)
		}
	}

	if err := svc.RecoverZone("zone-east", now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)
	if r := releaseByID(t, dir, rel.ID); r.Status != "succeeded" {
		t.Fatalf("release must proceed after zone recovery, got %s", r.Status)
	}
}

func TestZoneFailureRollsBackInFlightBatch(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 150*time.Millisecond) // batch 1 updated, health pending
	if err := svc.FailZone("zone-east", now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)
	if r := releaseByID(t, dir, rel.ID); r.Status != "rolled_back" {
		t.Fatalf("zone failure must roll back the in-flight batch, got %s", r.Status)
	}
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion != "v1.0.0" {
				t.Fatalf("instance %s = %s, want v1.0.0 after zone-failure rollback", inst.ID, inst.CurrentVersion)
			}
		}
	})
}

func batchesOf(t *testing.T, dir, releaseID string) []store.Batch {
	t.Helper()
	var out []store.Batch
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, b := range tx.Batches() {
			if b.ReleaseID == releaseID {
				out = append(out, b)
			}
		}
	})
	return out
}
