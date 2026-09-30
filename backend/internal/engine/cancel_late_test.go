package engine_test

import (
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// Scenario 7: cancel arrives while the first batch is updating/observing.
// Health results that land after the batch already entered rollback are
// stale and must neither flip the member healthy/unhealthy nor unblock the
// next batch.
func TestCancelThenLateHealthResults(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Fast updates/health, slower rollback: health answers can land while
	// rollback ops are already in flight.
	svc := engine.NewService(st, engine.Config{
		UpdateDelay:   40 * time.Millisecond,
		HealthDelay:   60 * time.Millisecond,
		RollbackDelay: 200 * time.Millisecond,
	})
	if err := svc.Seed(t0); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}

	now := t0
	// Run just past the update resolving so instances sit on v2, then cancel
	// before health answers land.
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Millisecond)
		if err := svc.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.CancelRelease(rel.ID, now); err != nil {
		t.Fatal(err)
	}

	var ops []store.Op
	st.View(func(tx *store.Tx) { ops = append([]store.Op(nil), tx.Ops()...) })
	sawHealth := false
	for _, op := range ops {
		if op.Kind == "health" {
			sawHealth = true
		}
	}
	if !sawHealth {
		t.Fatal("test setup expected pending health ops at cancel time")
	}

	for i := 0; i < 80; i++ {
		now = now.Add(30 * time.Millisecond)
		if err := svc.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
	}

	var r store.Release
	var batches []store.Batch
	var insts []store.Instance
	var remainOps []store.Op
	st.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		batches = append([]store.Batch(nil), tx.Batches()...)
		insts = append([]store.Instance(nil), tx.Instances()...)
		remainOps = append([]store.Op(nil), tx.Ops()...)
	})
	if r.Status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", r.Status)
	}
	for _, b := range batches {
		if b.Phase != "cancelled" {
			t.Fatalf("batch %d = %s, want cancelled", b.Index, b.Phase)
		}
	}
	if len(remainOps) != 0 {
		t.Fatalf("stranded ops: %d", len(remainOps))
	}
	for _, inst := range insts {
		if inst.CurrentVersion != "v1.0.0" || inst.Health != "healthy" {
			t.Fatalf("instance %s = %s/%s, want v1.0.0/healthy", inst.ID, inst.CurrentVersion, inst.Health)
		}
	}
	// Late health answers must not be recorded as authoritative healthy.
	for _, m := range batches[0].Members {
		if m.HealthState == "healthy" {
			t.Fatalf("member %s health must not be healthy after cancel-rollback", m.InstanceID)
		}
		if m.UpdateState != "rolled_back" {
			t.Fatalf("member %s updateState = %s, want rolled_back", m.InstanceID, m.UpdateState)
		}
	}
}
