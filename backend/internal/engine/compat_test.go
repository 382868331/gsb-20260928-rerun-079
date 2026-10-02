package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// A provider release whose dependent must update first is blocked before any
// batch expands; once the dependent is updated, review-continue proceeds.
func TestUpdateFirstBlocksUntilDependentUpdated(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	// Give the payment service two virtual instances on the baseline.
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}

	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v2.0.0", BatchSize: 1,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, time.Second)

	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		var rel store.Release
		for _, r := range tx.Releases() {
			if r.ID == payRel.ID {
				rel = r
			}
		}
		if rel.Status != "paused" || !rel.CompatBlocked {
			t.Fatalf("pay release should be compat-blocked, got status=%s blocked=%v", rel.Status, rel.CompatBlocked)
		}
		for _, b := range tx.Batches() {
			if b.ReleaseID == payRel.ID && b.Phase != "pending" {
				t.Fatalf("blocked batch must not expand, got %s", b.Phase)
			}
		}
		// The first (evaluated) batch must record its preflight basis.
		var first *store.Batch
		for i := range tx.Batches() {
			if tx.Batches()[i].ReleaseID == payRel.ID && tx.Batches()[i].Index == 0 {
				first = &tx.Batches()[i]
			}
		}
		if first == nil || first.Preflight == nil {
			t.Fatal("blocked batch must record its preflight basis")
		}
		// Payment instances never moved.
		for _, inst := range tx.Instances() {
			if inst.ServiceID == "svc-pay" && inst.CurrentVersion != "v1.0.0" {
				t.Fatalf("pay instance moved while blocked: %s", inst.CurrentVersion)
			}
		}
	})

	// Continue must be rejected while the constraint is still unmet.
	if err := svc.ReviewContinue(payRel.ID, now); err == nil {
		t.Fatal("continue must be rejected while the dependent is still old")
	}

	// Update the dependent (order service) first; concurrent releases across
	// different services are allowed.
	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 3,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == orderRel.ID && r.Status != "succeeded" {
				t.Fatalf("order release = %s, want succeeded", r.Status)
			}
		}
	})

	// Now the constraint is satisfied; continue the payment release.
	if err := svc.ReviewContinue(payRel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == payRel.ID && r.Status != "succeeded" {
				t.Fatalf("pay release after dependent update = %s, want succeeded", r.Status)
			}
		}
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion != "v2.0.0" {
				t.Fatalf("instance %s = %s, want v2.0.0", inst.ID, inst.CurrentVersion)
			}
		}
	})
}

// The order-service release records a stay_old finding: the payment service
// keeps its old version and stays compatible.
func TestStayOldRecordedInPreflight(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 3,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		found := false
		for _, b := range tx.Batches() {
			if b.ReleaseID != rel.ID || b.Preflight == nil {
				continue
			}
			for _, f := range b.Preflight.Findings {
				if f.Mode == "stay_old" && f.Compatible && strings.Contains(f.Detail, "保持旧版") {
					found = true
				}
			}
			if len(b.Preflight.HealthBasis) == 0 {
				t.Fatal("preflight must record the health basis of the batch")
			}
		}
		if !found {
			t.Fatal("expected a stay_old compatible finding on the order release batches")
		}
		for _, r := range tx.Releases() {
			if r.ID == rel.ID && r.Status != "succeeded" {
				t.Fatalf("order release = %s, want succeeded", r.Status)
			}
		}
	})
}

// A pause-mode constraint freezes the release for review before expanding;
// a human continue then lets it proceed.
func TestPauseModeConstraintReview(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.AddInstance("svc-inventory", now); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-inventory", TargetVersion: "v2.0.0", BatchSize: 1,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == rel.ID && r.Status != "paused" {
				t.Fatalf("inventory release = %s, want paused for compat review", r.Status)
			}
		}
	})
	if err := svc.ReviewContinue(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == rel.ID && r.Status != "succeeded" {
				t.Fatalf("inventory release after review = %s, want succeeded", r.Status)
			}
		}
	})
}

// Choosing rollback records which completed batches cannot be undone and
// whether dependents stay compatible.
func TestRollbackAssessmentNote(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "instances", Instances: []string{"inst-3"}, PauseOnFailure: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Let batch 1 succeed and batch 2 fail into a review pause.
	runUntil(t, svc, &now, 5*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == rel.ID && r.Status != "paused" {
				t.Fatalf("release = %s, want paused", r.Status)
			}
		}
	})
	if err := svc.ReviewRollback(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID != rel.ID {
				continue
			}
			if !strings.Contains(r.RollbackNote, "不能直接撤销") {
				t.Fatalf("rollback note must list completed batches, got %q", r.RollbackNote)
			}
			if !strings.Contains(r.RollbackNote, "兼容") {
				t.Fatalf("rollback note must address dependent compatibility, got %q", r.RollbackNote)
			}
		}
	})
	runUntil(t, svc, &now, 5*time.Second)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == rel.ID && r.Status != "rolled_back" {
				t.Fatalf("release = %s, want rolled_back", r.Status)
			}
		}
		// Batch 1 succeeded before the failure and is not undone: its
		// instances keep the version they really adopted.
		kept := 0
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion == "v2.0.0" {
				kept++
			}
		}
		if kept != 2 {
			t.Fatalf("completed batch instances must keep v2.0.0, got %d", kept)
		}
	})
}
