package engine_test

import (
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// On a failure-driven pause the operator cannot "continue" or "shrink"
// past unhealthy evidence; rollback or finish are the only ways out, and
// the made decision survives a store reopen without re-running the batch.
func TestFailurePauseRejectsContinueAndShrink(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "half_batch", PauseOnFailure: true},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	runUntil(t, svc, &now, 2*time.Second)

	if err := svc.ReviewContinue(rel.ID, now); err == nil {
		t.Fatal("continue must be rejected with unhealthy evidence")
	}
	if err := svc.ReviewShrink(rel.ID, 1, now); err == nil {
		t.Fatal("shrink must be rejected with unhealthy evidence")
	}

	// Reopen before deciding: paused state and evidence persist as-is.
	svc2 := newService(t, dir)
	if err := svc2.ReviewRollback(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc2, &now, 2*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		r := tx.Releases()[0]
		if r.Status != "rolled_back" || r.ReviewAction != "rollback" {
			t.Fatalf("status=%s action=%s", r.Status, r.ReviewAction)
		}
		for _, b := range tx.Batches() {
			if b.Index > 0 && b.Phase != "pending" {
				t.Fatalf("later batch %d expanded: %s", b.Index, b.Phase)
			}
		}
	})
}
