package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// The dependent_first rule blocks every order batch until the payment
// service has completed its own update; the evidence is persisted on the
// waiting batch and the release proceeds once payment is upgraded.
func TestDependentFirstBlocksOrderUntilPaymentUpdates(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	runUntil(t, svc, &now, time.Second)

	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		var r store.Release
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		if r.Status == "succeeded" {
			t.Fatal("order release must not proceed while payment is on the old version")
		}
		var b0 *store.Batch
		for i := range tx.Batches() {
			if tx.Batches()[i].ReleaseID == rel.ID && tx.Batches()[i].Index == 0 {
				b0 = &tx.Batches()[i]
			}
		}
		if b0 == nil || b0.Phase != "pending" {
			t.Fatalf("first batch must wait at the compat gate, got %+v", b0)
		}
		if !strings.Contains(b0.BlockedReason, "支付服务") {
			t.Fatalf("blocked reason must name the dependent service: %s", b0.BlockedReason)
		}
		if len(b0.Compat) == 0 {
			t.Fatal("compat evidence must be persisted on the waiting batch")
		}
	})

	// Upgrade payment: the gate must open and the release must proceed.
	upgradePayment(t, svc, &now)
	runUntil(t, svc, &now, 3*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		var r store.Release
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		if r.Status != "succeeded" {
			t.Fatalf("after payment update, order release = %s, want succeeded", r.Status)
		}
		for _, b := range tx.Batches() {
			if b.ReleaseID != rel.ID {
				continue
			}
			if b.Phase != "succeeded" {
				t.Fatalf("batch %d = %s, want succeeded", b.Index, b.Phase)
			}
			for _, c := range b.Compat {
				if !c.OK {
					t.Fatalf("batch %d expanded with a failing check: %+v", b.Index, c)
				}
			}
		}
	})
}

// The pause rule: while the order release advances, a notify release must
// wait; once order finishes, notify proceeds. The two must not deadlock.
func TestPauseRuleBlocksNotifyWhileOrderAdvances(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	upgradePayment(t, svc, &now)
	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Let the order release start advancing.
	runUntil(t, svc, &now, 300*time.Millisecond)
	notifyRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "ntf-v2.0.0", BatchSize: 1, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// A short window: notify must stay behind the gate while order advances.
	runUntil(t, svc, &now, 300*time.Millisecond)
	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		for _, b := range tx.Batches() {
			if b.ReleaseID == notifyRel.ID && b.Phase != "pending" {
				t.Fatalf("notify batch advanced while order was updating: %s", b.Phase)
			}
		}
	})
	// Both releases must eventually succeed (no deadlock between the gates).
	runUntil(t, svc, &now, 5*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		statuses := map[string]string{}
		for _, rr := range tx.Releases() {
			statuses[rr.ID] = rr.Status
		}
		if statuses[orderRel.ID] != "succeeded" || statuses[notifyRel.ID] != "succeeded" {
			t.Fatalf("order=%s notify=%s, want both succeeded", statuses[orderRel.ID], statuses[notifyRel.ID])
		}
	})
}

// The stay_old rule: notify must not update while payment is updating.
func TestStayOldBlocksNotifyWhilePaymentUpdates(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "pay-v2.0.0", BatchSize: 1, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 100*time.Millisecond)
	notifyRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "ntf-v2.0.0", BatchSize: 1, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 300*time.Millisecond)
	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		for _, b := range tx.Batches() {
			if b.ReleaseID == notifyRel.ID && b.Phase != "pending" {
				t.Fatalf("notify batch advanced while payment was updating: %s", b.Phase)
			}
			if b.ReleaseID == notifyRel.ID && b.Index == 0 && !strings.Contains(b.BlockedReason, "旧版") {
				t.Fatalf("blocked reason should mention staying on the old version: %s", b.BlockedReason)
			}
		}
	})
	runUntil(t, svc, &now, 5*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		statuses := map[string]string{}
		for _, rr := range tx.Releases() {
			statuses[rr.ID] = rr.Status
		}
		if statuses[payRel.ID] != "succeeded" || statuses[notifyRel.ID] != "succeeded" {
			t.Fatalf("payment=%s notify=%s, want both succeeded", statuses[payRel.ID], statuses[notifyRel.ID])
		}
	})
}

// Choosing rollback from a review pause records the impact: which finished
// batches cannot be undone and whether dependents stay compatible. The
// finished batch keeps its instances on the new version.
func TestRollbackImpactExplainsFinishedBatchesAndDependents(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	upgradePayment(t, svc, &now)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Let the first batch finish and the second batch start, then pause.
	for i := 0; i < 200; i++ {
		now = now.Add(25 * time.Millisecond)
		if err := svc.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
		done := false
		snapshotStore(t, dir).View(func(tx *store.Tx) {
			for _, b := range tx.Batches() {
				if b.ReleaseID == rel.ID && b.Index == 0 && b.Phase == "succeeded" {
					done = true
				}
			}
		})
		if done {
			break
		}
	}
	if err := svc.PauseForReview(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReviewRollback(rel.ID, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		var r store.Release
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		if len(r.RollbackImpact) == 0 {
			t.Fatal("rollback impact must be recorded on the release")
		}
		joined := strings.Join(r.RollbackImpact, "\n")
		if !strings.Contains(joined, "第 1 批已生效") {
			t.Fatalf("impact must list the finished batch that cannot be revoked: %s", joined)
		}
		if !strings.Contains(joined, "支付服务") {
			t.Fatalf("impact must explain dependent compatibility: %s", joined)
		}
	})

	runUntil(t, svc, &now, 3*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		var r store.Release
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		if r.Status != "rolled_back" {
			t.Fatalf("status = %s, want rolled_back", r.Status)
		}
		// The finished first batch is not revoked: its instances keep v2.
		onV2 := 0
		for _, inst := range tx.Instances() {
			if inst.ServiceID == "order" && inst.CurrentVersion == "v2.0.0" {
				onV2++
			}
		}
		if onV2 != 2 {
			t.Fatalf("finished batch must stay in effect: %d order instances on v2.0.0, want 2", onV2)
		}
		for _, b := range tx.Batches() {
			if b.ReleaseID == rel.ID && b.Index == 0 && b.Phase != "succeeded" {
				t.Fatalf("finished batch must not be revoked: %s", b.Phase)
			}
		}
	})
}
