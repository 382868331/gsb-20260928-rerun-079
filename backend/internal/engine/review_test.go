package engine_test

import (
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// Manual pause while a healthy release is in flight: evidence keeps arriving,
// then "continue" resumes and the release succeeds without re-running batches.
func TestManualPauseReviewContinue(t *testing.T) {
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

	runUntil(t, svc, &now, 150*time.Millisecond)
	if err := svc.PauseForReview(rel.ID, now); err != nil {
		t.Fatal(err)
	}

	// Tick for a while while paused: health results arrive but nothing
	// expands and the release stays frozen on the decision.
	for i := 0; i < 30; i++ {
		now = now.Add(50 * time.Millisecond)
		if err := svc.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
	}
	st := snapshotStore(t, dir)
	var r0 store.Release
	var batches []store.Batch
	st.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r0 = rr
			}
		}
		batches = append([]store.Batch(nil), tx.Batches()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if r0.Status != "paused" || !r0.PauseRequested {
		t.Fatalf("want paused+pauseRequested, got %s/%v", r0.Status, r0.PauseRequested)
	}
	if batches[1].Phase != "pending" || batches[2].Phase != "pending" {
		t.Fatal("later batches must not expand during review")
	}

	// Reopen: pause and review choice (not yet made) must survive.
	svc2 := newService(t, dir)
	if err := svc2.ReviewContinue(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	// A duplicate decision is rejected.
	if err := svc2.ReviewContinue(rel.ID, now); err == nil {
		t.Fatal("duplicate review decision must be rejected")
	}
	runUntil(t, svc2, &now, 2*time.Second)

	var insts []store.Instance
	st2 := snapshotStore(t, dir)
	st2.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r0 = rr
			}
		}
		batches = append([]store.Batch(nil), tx.Batches()...)
		insts = append([]store.Instance(nil), tx.Instances()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if r0.Status != "succeeded" {
		t.Fatalf("status = %s, want succeeded", r0.Status)
	}
	for _, b := range batches {
		if b.Phase != "succeeded" {
			t.Fatalf("batch %d = %s, want succeeded", b.Index, b.Phase)
		}
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v2.0.0" {
			t.Fatalf("instance %s = %s", inst.ID, inst.CurrentVersion)
		}
	}
}

// Manual pause -> shrink: later batches are rebuilt at the smaller size and
// the already-started batch is never re-executed.
func TestManualPauseReviewShrink(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	upgradePayment(t, svc, &now)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 3, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 150*time.Millisecond)
	if err := svc.PauseForReview(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		now = now.Add(50 * time.Millisecond)
		_ = svc.Engine().Tick(now)
	}
	if err := svc.ReviewShrink(rel.ID, 1, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)

	var r0 store.Release
	var batches []store.Batch
	var insts []store.Instance
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r0 = rr
			}
		}
		batches = append([]store.Batch(nil), tx.Batches()...)
		insts = append([]store.Instance(nil), tx.Instances()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if r0.Status != "succeeded" {
		t.Fatalf("status = %s", r0.Status)
	}
	if len(batches) != 4 {
		t.Fatalf("batches = %d, want 4 (one size-3 + three size-1)", len(batches))
	}
	if len(batches[0].Members) != 3 {
		t.Fatalf("first batch members = %d, must keep original 3", len(batches[0].Members))
	}
	for i := 1; i < 4; i++ {
		if len(batches[i].Members) != 1 {
			t.Fatalf("batch %d members = %d, want 1", i, len(batches[i].Members))
		}
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v2.0.0" {
			t.Fatalf("instance %s = %s", inst.ID, inst.CurrentVersion)
		}
	}
}

// Review rollback: a health result landing after the rollback decision is
// kept on the member as late evidence but cannot change the rolled-back fate.
func TestReviewRollbackLateEvidence(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.Config{
		UpdateDelay: 40 * time.Millisecond, HealthDelay: 400 * time.Millisecond,
		RollbackDelay: 60 * time.Millisecond,
	})
	if err := svc.Seed(t0); err != nil {
		t.Fatal(err)
	}
	now := t0
	upgradePayment(t, svc, &now)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Millisecond)
		_ = svc.Engine().Tick(now)
	}
	if err := svc.PauseForReview(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReviewRollback(rel.ID, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Run well past the health delay: late answers arrive after rollback.
	for i := 0; i < 60; i++ {
		now = now.Add(30 * time.Millisecond)
		_ = svc.Engine().Tick(now)
	}

	var r0 store.Release
	var batches []store.Batch
	var insts []store.Instance
	var ops []store.Op
	st.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r0 = rr
			}
		}
		batches = append([]store.Batch(nil), tx.Batches()...)
		insts = append([]store.Instance(nil), tx.Instances()...)
		ops = append([]store.Op(nil), tx.Ops()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if r0.Status != "rolled_back" {
		t.Fatalf("status = %s, want rolled_back", r0.Status)
	}
	if len(ops) != 0 {
		t.Fatalf("stranded ops: %d", len(ops))
	}
	lateEvidence := 0
	for _, m := range batches[0].Members {
		if m.UpdateState != "rolled_back" {
			t.Fatalf("member %s = %s", m.InstanceID, m.UpdateState)
		}
		if m.LateHealthResult != "" {
			lateEvidence++
			if m.LateHealthVersion != "v2.0.0" {
				t.Fatalf("late evidence version = %s", m.LateHealthVersion)
			}
			if m.HealthState == "healthy" {
				t.Fatal("late answer must not flip member to authoritative healthy")
			}
		}
	}
	if lateEvidence == 0 {
		t.Fatal("expected late health evidence to remain queryable after rollback")
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v1.0.0" || inst.Health != "healthy" {
			t.Fatalf("instance %s = %s/%s", inst.ID, inst.CurrentVersion, inst.Health)
		}
	}
}

// Cancel after a manual pause keeps the decision terminal; a later health
// answer is recorded but the release ends cancelled.
func TestManualPauseCancelLateEvidence(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.Config{
		UpdateDelay: 40 * time.Millisecond, HealthDelay: 400 * time.Millisecond,
		RollbackDelay: 60 * time.Millisecond,
	})
	if err := svc.Seed(t0); err != nil {
		t.Fatal(err)
	}
	now := t0
	upgradePayment(t, svc, &now)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Millisecond)
		_ = svc.Engine().Tick(now)
	}
	if err := svc.PauseForReview(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelRelease(rel.ID, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		now = now.Add(30 * time.Millisecond)
		_ = svc.Engine().Tick(now)
	}
	var r0 store.Release
	var insts []store.Instance
	st.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r0 = rr
			}
		}
		insts = append([]store.Instance(nil), tx.Instances()...)
	})
	if r0.Status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", r0.Status)
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s = %s", inst.ID, inst.CurrentVersion)
		}
	}
	// The decision must remain immutable: no review action is accepted after.
	if err := svc.ReviewContinue(rel.ID, now); err == nil {
		t.Fatal("review continue must be rejected after cancel")
	}
}
