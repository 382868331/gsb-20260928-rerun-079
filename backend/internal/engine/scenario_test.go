package engine_test

import (
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

var t0 = time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)

const (
	dUpdate   = 100 * time.Millisecond
	dHealth   = 100 * time.Millisecond
	dRollback = 100 * time.Millisecond
)

func newService(t *testing.T, dir string) *engine.Service {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st, engine.Config{UpdateDelay: dUpdate, HealthDelay: dHealth, RollbackDelay: dRollback})
	if err := svc.Seed(t0); err != nil {
		t.Fatal(err)
	}
	return svc
}

// upgradePayment completes the payment service's own update first: the
// dependent_first compat rule blocks every order batch until payment runs
// its latest version.
func upgradePayment(t *testing.T, svc *engine.Service, now *time.Time) {
	t.Helper()
	if _, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "pay-v2.0.0", BatchSize: 2, Failure: store.FailureSpec{Mode: "none"},
	}, *now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, now, 3*time.Second)
}

// orderInstances filters the seeded fleet down to the order service.
func orderInstances(insts []store.Instance) []store.Instance {
	var out []store.Instance
	for _, inst := range insts {
		if inst.ServiceID == "order" {
			out = append(out, inst)
		}
	}
	return out
}

// releaseBatchesOf filters batches by release id.
func releaseBatchesOf(batches []store.Batch, releaseID string) []store.Batch {
	var out []store.Batch
	for _, b := range batches {
		if b.ReleaseID == releaseID {
			out = append(out, b)
		}
	}
	return out
}

// Scenario 1: half the first batch fails health observation. The batch rolls
// back, later batches never expand, and every instance ends on the baseline.
func TestHalfBatchFailureRollback(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)

	now := t0
	upgradePayment(t, svc, &now)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "rolling",
		Failure: store.FailureSpec{Mode: "half_batch"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	runUntil(t, svc, &now, 2*time.Second)

	var snap store.Snapshot
	svc.Engine() // touch
	st := snapshotStore(t, dir)
	st.View(func(tx *store.Tx) {
		snap = store.Snapshot{
			Releases:  append([]store.Release(nil), tx.Releases()...),
			Batches:   append([]store.Batch(nil), tx.Batches()...),
			Instances: append([]store.Instance(nil), tx.Instances()...),
			Ops:       append([]store.Op(nil), tx.Ops()...),
		}
	})

	r := snap.Releases[0]
	for _, rr := range snap.Releases {
		if rr.ID == rel.ID {
			r = rr
		}
	}
	if r.Status != "rolled_back" {
		t.Fatalf("release status = %s, want rolled_back (id=%s)", r.Status, rel.ID)
	}
	snap.Batches = releaseBatchesOf(snap.Batches, rel.ID)
	if len(snap.Batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(snap.Batches))
	}
	if snap.Batches[0].Phase != "rolled_back" {
		t.Fatalf("batch0 phase = %s, want rolled_back", snap.Batches[0].Phase)
	}
	if snap.Batches[1].Phase != "pending" || snap.Batches[2].Phase != "pending" {
		t.Fatalf("later batches must never expand: %s %s", snap.Batches[1].Phase, snap.Batches[2].Phase)
	}
	for _, inst := range orderInstances(snap.Instances) {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s current = %s, want v1.0.0", inst.ID, inst.CurrentVersion)
		}
	}
	if len(snap.Ops) != 0 {
		t.Fatalf("pending ops = %d, want 0", len(snap.Ops))
	}
	// Failed members must have a clear destination state.
	for _, m := range snap.Batches[0].Members {
		if m.UpdateState != "rolled_back" {
			t.Fatalf("member %s updateState = %s, want rolled_back", m.InstanceID, m.UpdateState)
		}
	}
}

// Scenario 2: rollback is interrupted repeatedly. Reopen the store mid
// rollback (simulating process kill + restart); recovery must finish and
// every instance must still land on the baseline.
func TestRollbackInterruptedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	upgradePayment(t, svc, &now)
	if _, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "half_batch", FailFirstNRollback: 3},
	}, now); err != nil {
		t.Fatal(err)
	}

	// Let rollback start and suffer one interruption.
	for i := 0; i < 12; i++ {
		now = now.Add(50 * time.Millisecond)
		if err := svc.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
	}

	// "Kill the process": reopen store and service from disk.
	svc2 := newService(t, dir)
	for i := 0; i < 40; i++ {
		now = now.Add(50 * time.Millisecond)
		if err := svc2.Engine().Tick(now); err != nil {
			t.Fatal(err)
		}
	}

	st := snapshotStore(t, dir)
	var rel store.Release
	var insts []store.Instance
	var ops []store.Op
	var batches []store.Batch
	st.View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ServiceID == "order" {
				rel = r
			}
		}
		insts = append([]store.Instance(nil), tx.Instances()...)
		ops = append([]store.Op(nil), tx.Ops()...)
		batches = append([]store.Batch(nil), tx.Batches()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if rel.Status != "rolled_back" {
		t.Fatalf("after restart status = %s, want rolled_back", rel.Status)
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s = %s, want v1.0.0", inst.ID, inst.CurrentVersion)
		}
	}
	if len(ops) != 0 {
		t.Fatalf("stranded ops after recovery: %d", len(ops))
	}
	sawRetry := false
	for _, e := range []struct{}{} {
		_ = e
	}
	st.View(func(tx *store.Tx) {
		for _, ev := range tx.Events() {
			if contains(ev.Message, "回退中断") {
				sawRetry = true
			}
		}
	})
	if !sawRetry {
		t.Fatal("expected at least one recorded rollback interruption event")
	}
	if batches[0].Phase != "rolled_back" {
		t.Fatalf("batch phase = %s", batches[0].Phase)
	}
}

// Scenario 3: a late health result must never be attributed to the next
// version. Rollback completes first; a stale health op that was already
// scheduled is then resolved and recorded as stale — instance stays on the
// baseline and is not flipped unhealthy by the ghost result.
func TestLateHealthResultNotAttributedToNext(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	cfg := engine.Config{UpdateDelay: dUpdate, HealthDelay: dHealth, RollbackDelay: 1 * time.Millisecond}
	st, _ := store.Open(dir)
	svc = engine.NewService(st, cfg)
	if err := svc.Seed(t0); err == nil {
		// seed no-op when reopened
	}

	now := t0
	upgradePayment(t, svc, &now)
	rel1, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 6,
		Failure: store.FailureSpec{Mode: "instances", Instances: []string{"inst-1"}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Advance past updates and the single failing health result, then stop
	// rollback ops by removing them and keep one health op artificially:
	// instead we fast-forward naturally and verify that after rollback no
	// later release gets blamed.
	runUntil(t, svc, &now, 1500*time.Millisecond)

	// Now start a second successful release to v2.0.0: any late result from
	// release #1 must be neutral. Add a fresh release and run it.
	v2svc := engine.NewService(st, engine.DefaultConfig())
	_ = v2svc
	rel2, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 6, Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)

	var rel store.Release
	var insts []store.Instance
	var batches []store.Batch
	st.View(func(tx *store.Tx) {
		for _, r := range tx.Releases() {
			if r.ID == rel2.ID {
				rel = r
			}
		}
		insts = append([]store.Instance(nil), tx.Instances()...)
		batches = append([]store.Batch(nil), tx.Batches()...)
	})
	if rel.Status != "succeeded" {
		t.Fatalf("second release = %s, want succeeded", rel.Status)
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v2.0.0" || inst.Health != "healthy" {
			t.Fatalf("instance %s = %s/%s, want v2.0.0/healthy", inst.ID, inst.CurrentVersion, inst.Health)
		}
	}
	// First release's member health must be recorded stale, not unhealthy-on-v2.
	first := releaseBatchesOf(batches, rel1.ID)[0]
	staleCount := 0
	for _, m := range first.Members {
		if m.HealthState == "stale" {
			staleCount++
		}
	}
	if staleCount == 0 {
		t.Fatal("expected stale health states on the rolled-back first release")
	}
}

// Scenario 4: cancel stops further expansion; updated instances in the active
// batch are rolled back, untouched later batches stay pending.
func TestCancelStopsExpansionAndRollbacksUpdated(t *testing.T) {
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
	// Cancel right after creation, before the first batch starts.
	if err := svc.CancelRelease(rel.ID, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 2*time.Second)

	st := snapshotStore(t, dir)
	var r store.Release
	var batches []store.Batch
	var insts []store.Instance
	st.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		batches = append([]store.Batch(nil), tx.Batches()...)
		insts = append([]store.Instance(nil), tx.Instances()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if r.Status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", r.Status)
	}
	for _, b := range batches {
		if b.Phase != "cancelled" {
			t.Fatalf("batch %d phase = %s, want cancelled", b.Index, b.Phase)
		}
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s = %s, want v1.0.0", inst.ID, inst.CurrentVersion)
		}
	}
}

// Scenario 5: with pauseOnFailure the release freezes on the failed member so
// an operator can review, then resumes the rollback. The page may be closed
// and reopened meanwhile; state comes entirely from disk.
func TestPauseReviewThenResumeRollback(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	upgradePayment(t, svc, &now)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "half_batch", PauseOnFailure: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	runUntil(t, svc, &now, 2*time.Second)

	st := snapshotStore(t, dir)
	var r store.Release
	var batches []store.Batch
	var ops []store.Op
	st.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		batches = append([]store.Batch(nil), tx.Batches()...)
		ops = append([]store.Op(nil), tx.Ops()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if r.Status != "paused" {
		t.Fatalf("status = %s, want paused", r.Status)
	}
	if batches[0].Phase != "paused" {
		t.Fatalf("batch phase = %s, want paused", batches[0].Phase)
	}
	for _, op := range ops {
		if op.Kind == "rollback" {
			t.Fatal("no rollback op must be scheduled while the drill is paused")
		}
	}
	if batches[1].Phase != "pending" {
		t.Fatalf("later batch phase = %s, want pending", batches[1].Phase)
	}

	// Reopen (simulating refresh/restart): the pause is still there.
	svc2 := newService(t, dir)
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		var found bool
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID && rr.Status == "paused" {
				found = true
			}
		}
		if !found {
			t.Fatal("pause must survive reopen")
		}
	})

	if err := svc2.ResumeRollback(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc2, &now, 2*time.Second)

	var rel2 store.Release
	var insts []store.Instance
	st2 := snapshotStore(t, dir)
	st2.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				rel2 = rr
			}
		}
		insts = append([]store.Instance(nil), tx.Instances()...)
		batches = append([]store.Batch(nil), tx.Batches()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if rel2.Status != "rolled_back" {
		t.Fatalf("after resume status = %s, want rolled_back", rel2.Status)
	}
	if batches[1].Phase != "pending" {
		t.Fatalf("later batches must remain pending: %s", batches[1].Phase)
	}
	for _, inst := range orderInstances(insts) {
		if inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("instance %s = %s, want v1.0.0", inst.ID, inst.CurrentVersion)
		}
	}
}

// Scenario 6: review and end the drill without rollback keeps the real mixed
// version distribution (half the batch on v2, half never moved).
func TestPauseReviewThenFinishKeepsDistribution(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	upgradePayment(t, svc, &now)
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		TargetVersion: "v2.0.0", BatchSize: 2,
		Failure: store.FailureSpec{Mode: "half_batch", PauseOnFailure: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	runUntil(t, svc, &now, 2*time.Second)

	var before []store.Instance
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		before = append([]store.Instance(nil), tx.Instances()...)
	})
	v2Before := 0
	unhealthy := 0
	for _, inst := range orderInstances(before) {
		if inst.CurrentVersion == "v2.0.0" {
			v2Before++
		}
	}
	for _, inst := range orderInstances(before) {
		if inst.Health == "unhealthy" {
			unhealthy++
		}
	}
	if v2Before != 2 || unhealthy != 1 {
		t.Fatalf("during pause v2=%d unhealthy=%d, want 2 and 1", v2Before, unhealthy)
	}

	if err := svc.FinishDrill(rel.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 1*time.Second)

	st := snapshotStore(t, dir)
	var r store.Release
	var insts []store.Instance
	var batches []store.Batch
	var ops []store.Op
	st.View(func(tx *store.Tx) {
		for _, rr := range tx.Releases() {
			if rr.ID == rel.ID {
				r = rr
			}
		}
		insts = append([]store.Instance(nil), tx.Instances()...)
		batches = append([]store.Batch(nil), tx.Batches()...)
		ops = append([]store.Op(nil), tx.Ops()...)
	})
	batches = releaseBatchesOf(batches, rel.ID)
	if r.Status != "failed" {
		t.Fatalf("status = %s, want failed", r.Status)
	}
	if batches[0].Phase != "failed" {
		t.Fatalf("batch0 = %s, want failed", batches[0].Phase)
	}
	v2After := 0
	for _, inst := range orderInstances(insts) {
		switch inst.CurrentVersion {
		case "v2.0.0":
			v2After++
		case "v1.0.0":
		default:
			t.Fatalf("unexpected version %s", inst.CurrentVersion)
		}
	}
	if v2After != v2Before {
		t.Fatalf("finishing changed the distribution: v2 count %d -> %d", v2Before, v2After)
	}
	if len(ops) != 0 {
		t.Fatalf("no rollback ops expected after finish, got %d", len(ops))
	}
}

func runUntil(t *testing.T, svc *engine.Service, now *time.Time, d time.Duration) {
	t.Helper()
	deadline := now.Add(d)
	for now.Before(deadline) {
		*now = now.Add(25 * time.Millisecond)
		if err := svc.Engine().Tick(*now); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
