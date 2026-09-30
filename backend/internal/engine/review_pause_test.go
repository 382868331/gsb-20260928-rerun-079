package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// Scenario 8: operator requests a review pause on a healthy release. The
// release freezes once the active batch's health evidence is complete; after
// "continue" the remaining batches roll out and the release succeeds. The
// already completed batch must never be re-executed.
func TestReviewPauseThenContinue(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestReviewPause(rel.ID, t0); err != nil {
		t.Fatal(err)
	}

	now := t0
	runUntil(t, svc, &now, 2*time.Second)

	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		r := tx.Releases()[0]
		if r.Status != "paused" || r.PauseKind != "review" {
			t.Fatalf("status = %s kind = %s, want paused/review", r.Status, r.PauseKind)
		}
		if tx.Batches()[0].Phase != "paused" {
			t.Fatalf("batch0 = %s, want paused", tx.Batches()[0].Phase)
		}
		for _, m := range tx.Batches()[0].Members {
			if m.HealthState != "healthy" {
				t.Fatalf("review evidence missing: %s health = %s", m.InstanceID, m.HealthState)
			}
		}
	})

	if err := svc.ContinueRelease(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)

	snapshotStore(t, dir).View(func(tx *store.Tx) {
		r := tx.Releases()[0]
		if r.Status != "succeeded" {
			t.Fatalf("status = %s, want succeeded", r.Status)
		}
		for _, b := range tx.Batches() {
			if b.Phase != "succeeded" {
				t.Fatalf("batch %d = %s, want succeeded", b.Index, b.Phase)
			}
		}
		// No update op may ever have targeted a batch-0 instance twice: each
		// instance appears in exactly one batch and batch 0 kept its members.
		seen := map[string]int{}
		for _, b := range tx.Batches() {
			for _, m := range b.Members {
				seen[m.InstanceID]++
			}
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("instance %s appears in %d batches, completed work was re-executed", id, n)
			}
		}
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion != "v2.0.0" {
				t.Fatalf("instance %s = %s, want v2.0.0", inst.ID, inst.CurrentVersion)
			}
		}
	})
}

// Scenario 9: review pause, then shrink the remaining batches and continue.
// Pending batches are rebuilt smaller; the completed batch is untouched.
func TestReviewPauseThenShrink(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestReviewPause(rel.ID, t0); err != nil {
		t.Fatal(err)
	}
	now := t0
	runUntil(t, svc, &now, 2*time.Second)

	st := snapshotStore(t, dir)
	var doneBatchID string
	st.View(func(tx *store.Tx) {
		if tx.Releases()[0].Status != "paused" {
			t.Fatalf("status = %s, want paused", tx.Releases()[0].Status)
		}
		doneBatchID = tx.Batches()[0].ID
	})

	if err := svc.ShrinkAndContinue(rel.ID, 1, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 8*time.Second)

	snapshotStore(t, dir).View(func(tx *store.Tx) {
		r := tx.Releases()[0]
		if r.Status != "succeeded" {
			t.Fatalf("status = %s, want succeeded", r.Status)
		}
		// 1 finished batch of 2 + 4 shrunk batches of 1.
		if len(tx.Batches()) != 5 {
			t.Fatalf("batches = %d, want 5 (1 kept + 4 shrunk)", len(tx.Batches()))
		}
		if tx.Batches()[0].ID != doneBatchID || len(tx.Batches()[0].Members) != 2 {
			t.Fatal("completed batch must be kept untouched")
		}
		for _, b := range tx.Batches()[1:] {
			if len(b.Members) != 1 {
				t.Fatalf("shrunk batch %d has %d members, want 1", b.Index, len(b.Members))
			}
			if b.Phase != "succeeded" {
				t.Fatalf("batch %d = %s, want succeeded", b.Index, b.Phase)
			}
		}
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion != "v2.0.0" {
				t.Fatalf("instance %s = %s, want v2.0.0", inst.ID, inst.CurrentVersion)
			}
		}
	})
}

// Scenario 10: review pause, then roll everything back. Instances that
// actually moved return to the baseline; pending batches are cancelled.
func TestReviewPauseThenRollbackAll(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestReviewPause(rel.ID, t0); err != nil {
		t.Fatal(err)
	}
	now := t0
	runUntil(t, svc, &now, 2*time.Second)

	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		if tx.Releases()[0].Status != "paused" {
			t.Fatalf("status = %s, want paused", tx.Releases()[0].Status)
		}
	})

	if err := svc.RollbackRelease(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)

	snapshotStore(t, dir).View(func(tx *store.Tx) {
		r := tx.Releases()[0]
		if r.Status != "rolled_back" {
			t.Fatalf("status = %s, want rolled_back", r.Status)
		}
		if tx.Batches()[0].Phase != "rolled_back" {
			t.Fatalf("batch0 = %s, want rolled_back", tx.Batches()[0].Phase)
		}
		for _, b := range tx.Batches()[1:] {
			if b.Phase != "cancelled" {
				t.Fatalf("batch %d = %s, want cancelled", b.Index, b.Phase)
			}
		}
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion != "v1.0.0" {
				t.Fatalf("instance %s = %s, want v1.0.0", inst.ID, inst.CurrentVersion)
			}
		}
		if len(tx.Ops()) != 0 {
			t.Fatalf("stranded ops: %d", len(tx.Ops()))
		}
	})
}

// Scenario 11: the process restarts while the release sits in a review
// pause. The pause, the review decision and the actually updated instances
// all survive on disk; continuing must not re-execute the completed batch.
func TestReviewPauseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestReviewPause(rel.ID, t0); err != nil {
		t.Fatal(err)
	}
	now := t0
	runUntil(t, svc, &now, 2*time.Second)

	// "Kill the process" mid-pause and reopen from disk.
	svc2 := newService(t, dir)
	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		r := tx.Releases()[0]
		if r.Status != "paused" || r.PauseKind != "review" {
			t.Fatalf("after restart status = %s kind = %s, want paused/review", r.Status, r.PauseKind)
		}
		updated := 0
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion == "v2.0.0" {
				updated++
			}
		}
		if updated != 2 {
			t.Fatalf("after restart %d instances on v2, want 2", updated)
		}
	})

	if err := svc2.ContinueRelease(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc2, &now, 5*time.Second)

	snapshotStore(t, dir).View(func(tx *store.Tx) {
		if tx.Releases()[0].Status != "succeeded" {
			t.Fatalf("status = %s, want succeeded", tx.Releases()[0].Status)
		}
		// The completed batch must still hold its original members with their
		// original health evidence: nothing was re-executed.
		first := tx.Batches()[0]
		for _, m := range first.Members {
			if m.HealthState != "healthy" || m.UpdateState != "succeeded" {
				t.Fatalf("completed member %s re-executed: %s/%s", m.InstanceID, m.UpdateState, m.HealthState)
			}
		}
		// Exactly one update op was ever recorded per instance (via events).
		updates := map[string]int{}
		for _, ev := range tx.Events() {
			if strings.Contains(ev.Message, "开始扩批更新") {
				continue
			}
			if strings.Contains(ev.Message, "已更新到") && ev.InstanceID != "" {
				updates[ev.InstanceID]++
			}
		}
		for id, n := range updates {
			if n != 1 {
				t.Fatalf("instance %s updated %d times, completed batch re-executed", id, n)
			}
		}
	})
}

// Scenario 12: a health result that arrives after the release already
// reached a terminal decision is recorded for audit but changes nothing.
func TestLateHealthAfterTerminalDecisionRecordedOnly(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.Config{
		UpdateDelay: 40 * time.Millisecond, HealthDelay: 60 * time.Millisecond, RollbackDelay: 30 * time.Millisecond,
	})
	if err := svc.Seed(t0); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "half_batch"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	runUntil(t, svc, &now, 3*time.Second)

	st.View(func(tx *store.Tx) {
		if tx.Releases()[0].Status != "rolled_back" {
			t.Fatalf("status = %s, want rolled_back", tx.Releases()[0].Status)
		}
	})

	// Inject a ghost health op belonging to the finished release and let it
	// resolve: it must be logged but must not mutate anything.
	var batchID string
	st.View(func(tx *store.Tx) { batchID = tx.Batches()[0].ID })
	if err := st.Mutate(func(tx *store.Tx) error {
		tx.SetOps(append(tx.Ops(), store.Op{
			ID: "op-ghost", Kind: "health", ReleaseID: rel.ID, BatchID: batchID,
			InstanceID: "inst-2", ToVersion: "v2.0.0", ResolveVersion: "v2.0.0",
			DueAt: now.Add(time.Millisecond).Format(time.RFC3339Nano),
		}))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 500*time.Millisecond)

	snapshotStore(t, dir).View(func(tx *store.Tx) {
		if tx.Releases()[0].Status != "rolled_back" {
			t.Fatalf("terminal decision changed: %s", tx.Releases()[0].Status)
		}
		for _, inst := range tx.Instances() {
			if inst.CurrentVersion != "v1.0.0" || inst.Health != "healthy" {
				t.Fatalf("late result mutated instance %s: %s/%s", inst.ID, inst.CurrentVersion, inst.Health)
			}
		}
		found := false
		for _, ev := range tx.Events() {
			if strings.Contains(ev.Message, "仅留档可查") {
				found = true
			}
		}
		if !found {
			t.Fatal("late health result after terminal decision must be recorded as an auditable event")
		}
	})
}
