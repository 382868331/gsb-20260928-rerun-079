package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// standoff sets up the mutual wait: the order release is paused for review
// while its third batch holds two zone-east instances, and the payment
// release's first batch cannot start without breaching the shared pool
// floor. Both plans wait on each other's next move.
type standoff struct {
	svc     *engine.Service
	orderID string
	payID   string
	now     time.Time
}

func setupStandoff(t *testing.T, dir string) standoff {
	t.Helper()
	svc := newServiceCfg(t, dir, slowHealthCfg)
	now := t0
	// Order: 5 east + 4 west after the extra instances; pay: 2 east + 2 west.
	addInstances(t, svc, "svc-order", 3, now)
	addInstances(t, svc, "svc-pay", 4, now)
	if _, err := svc.CreateVersion("v1.5.0", "支付服务 1.5.0", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddPool(store.SharedPool{
		Name: "订单支付共享池", ZoneID: "zone-east",
		ServiceIDs: []string{"svc-order", "svc-pay"}, MinAvailable: 4,
	}, now); err != nil {
		t.Fatal(err)
	}
	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 3,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Batches 1 and 2 succeed; batch 3 (inst-7, inst-8, inst-9) is observing
	// when we pause it, holding two zone-east instances.
	runUntil(t, svc, &now, 2500*time.Millisecond)
	if err := svc.PauseForReview(orderRel.ID, now); err != nil {
		t.Fatal(err)
	}
	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 4,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 200*time.Millisecond)

	r := releaseByID(t, dir, payRel.ID)
	if !r.CapacityBlocked {
		t.Fatalf("pay release must wait for the shared pool, got %+v", r)
	}
	if !strings.Contains(r.CapacityNote, orderRel.ID) {
		t.Fatalf("capacity note must name the holder release %s, got %q", orderRel.ID, r.CapacityNote)
	}
	// The prepared-but-not-yet-effective batch's commitment is queryable.
	view := capacityOf(t, dir)
	found := false
	for _, c := range view.Commitments {
		if c.ReleaseID == orderRel.ID && c.ZoneID == "zone-east" && c.State == "paused" && len(c.InstanceIDs) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("order's paused batch must hold a queryable 2-instance commitment, got %+v", view.Commitments)
	}
	// The wait is visible in the dependency/capacity ledger.
	deps := svc.DependencyStatuses()
	payWaiting := false
	for _, d := range deps {
		if d.ReleaseID == payRel.ID && d.Blocked {
			payWaiting = true
		}
	}
	if !payWaiting {
		t.Fatalf("pay release must appear as blocked in dependency status, got %+v", deps)
	}
	return standoff{svc: svc, orderID: orderRel.ID, payID: payRel.ID, now: now}
}

// Save at the moment both plans wait, reopen, then explicitly cancel one
// side: the other's completed batches stay untouched and the cancelled
// side's capacity frees the waiting plan.
func TestStandoffReopenThenCancelOneSide(t *testing.T) {
	dir := t.TempDir()
	s := setupStandoff(t, dir)

	// Reopen from disk: the standoff survives exactly as persisted.
	svc2 := newServiceCfg(t, dir, slowHealthCfg)
	if r := releaseByID(t, dir, s.payID); !r.CapacityBlocked {
		t.Fatal("capacity wait must survive a reopen")
	}
	if view := capacityOf(t, dir); len(view.Commitments) == 0 {
		t.Fatal("commitments must survive a reopen")
	}

	if err := svc2.CancelRelease(s.orderID, s.now); err != nil {
		t.Fatal(err)
	}
	// The cancelled side rolls its paused batch back. While the rollback
	// ops are still in flight the instances remain occupied: the waiting
	// plan must not treat not-yet-recovered instances as available.
	s.now = s.now.Add(50 * time.Millisecond)
	if err := svc2.Engine().Tick(s.now); err != nil {
		t.Fatal(err)
	}
	if r := releaseByID(t, dir, s.payID); !r.CapacityBlocked {
		t.Fatal("pay must keep waiting while the holder's rollback is still recovering instances")
	}
	if view := capacityOf(t, dir); len(view.Commitments) == 0 {
		t.Fatal("rollback_in_progress instances must still count as occupied")
	}

	runUntil(t, svc2, &s.now, 8*time.Second)
	if r := releaseByID(t, dir, s.orderID); r.Status != "cancelled" {
		t.Fatalf("order release = %s, want cancelled", r.Status)
	}
	if r := releaseByID(t, dir, s.payID); r.Status != "succeeded" {
		t.Fatalf("pay release = %s, want succeeded after the capacity freed", r.Status)
	}
	// Cancelling one side never undoes the other side's (or its own)
	// already-completed batches: order batches 1-2 keep v2.0.0, the rolled
	// back batch 3 returns to v1.0.0, pay reaches v1.5.0 everywhere.
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, inst := range tx.Instances() {
			switch {
			case inst.ServiceID == "svc-pay":
				if inst.CurrentVersion != "v1.5.0" {
					t.Fatalf("pay instance %s = %s, want v1.5.0", inst.ID, inst.CurrentVersion)
				}
			case inst.ID == "inst-7" || inst.ID == "inst-8" || inst.ID == "inst-9":
				if inst.CurrentVersion != "v1.0.0" {
					t.Fatalf("rolled-back instance %s = %s, want v1.0.0", inst.ID, inst.CurrentVersion)
				}
			default:
				if inst.CurrentVersion != "v2.0.0" {
					t.Fatalf("completed batch instance %s = %s, want v2.0.0 (cancel must not undo it)", inst.ID, inst.CurrentVersion)
				}
			}
		}
	})
}

// The same standoff can also be continued: the review completes the paused
// batch, its commitment frees, and the waiting plan proceeds.
func TestStandoffReopenThenContinue(t *testing.T) {
	dir := t.TempDir()
	s := setupStandoff(t, dir)

	svc2 := newServiceCfg(t, dir, slowHealthCfg)
	if err := svc2.ReviewContinue(s.orderID, s.now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc2, &s.now, 10*time.Second)
	if r := releaseByID(t, dir, s.orderID); r.Status != "succeeded" {
		t.Fatalf("order release = %s, want succeeded", r.Status)
	}
	if r := releaseByID(t, dir, s.payID); r.Status != "succeeded" {
		t.Fatalf("pay release = %s, want succeeded once capacity freed", r.Status)
	}
	if view := capacityOf(t, dir); len(view.Commitments) != 0 {
		t.Fatalf("no commitments may remain, got %+v", view.Commitments)
	}
}

// A compat cycle blocks both directions; the mutual wait is reported and
// survives a reopen. Cancelling one side ends its plan; the other side
// stays blocked until its dependency is genuinely satisfied.
func TestCompatMutualWaitReportedAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	addInstances(t, svc, "svc-pay", 2, now)
	if _, err := svc.AddRule(store.CompatRule{
		ProviderID: "svc-order", DependentID: "svc-pay", DependentVersion: "v1.0.0",
		MinProviderVersion: "v1.0.0", MaxProviderVersion: "v1.9.9",
		Mode: "update_first", Note: "支付 v1 仅兼容订单 v1.x",
	}, now); err != nil {
		t.Fatal(err)
	}
	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v2.0.0", BatchSize: 1,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, time.Second)

	mutual := func(deps []store.DependencyStatus) bool {
		m := map[string]bool{}
		for _, d := range deps {
			if d.Mutual {
				m[d.ReleaseID] = true
			}
		}
		return m[payRel.ID] && m[orderRel.ID]
	}
	if !mutual(svc.DependencyStatuses()) {
		t.Fatalf("both releases must be reported as mutually waiting, got %+v", svc.DependencyStatuses())
	}

	// Reopen: the deadlock is still there, and "continue" is still refused.
	svc2 := newService(t, dir)
	if !mutual(svc2.DependencyStatuses()) {
		t.Fatal("mutual wait must survive a reopen")
	}
	if err := svc2.ReviewContinue(orderRel.ID, now); err == nil {
		t.Fatal("continue must stay rejected while the dependency is unmet")
	}

	// Explicitly cancel one side: its plan ends, nothing else is undone.
	if err := svc2.CancelRelease(payRel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc2, &now, 2*time.Second)
	if r := releaseByID(t, dir, payRel.ID); r.Status != "cancelled" {
		t.Fatalf("pay release = %s, want cancelled", r.Status)
	}
	if r := releaseByID(t, dir, orderRel.ID); !r.CompatBlocked {
		t.Fatal("cancelling the other side does not fake the dependency: order must stay blocked")
	}

	// Genuinely satisfy the dependency (update pay to a compatible
	// intermediate), then the survivor continues.
	if _, err := svc2.CreateVersion("v1.5.0", "支付服务 1.5.0", now); err != nil {
		t.Fatal(err)
	}
	payMid, err := svc2.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc2, &now, 5*time.Second)
	if r := releaseByID(t, dir, payMid.ID); r.Status != "succeeded" {
		t.Fatalf("pay intermediate release = %s, want succeeded", r.Status)
	}
	if err := svc2.ReviewContinue(orderRel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc2, &now, 5*time.Second)
	if r := releaseByID(t, dir, orderRel.ID); r.Status != "succeeded" {
		t.Fatalf("order release = %s, want succeeded after dependency satisfied", r.Status)
	}
}
