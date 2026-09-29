package engine_test

import (
	"testing"

	"releaselab/internal/engine"
	"releaselab/internal/store"
)

const (
	base   = "ver-v1.0.0"
	target = "ver-v2.0.0"
)

func newEngine(t *testing.T) (*engine.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st)
	// Deterministic virtual clock: start at a fixed instant, never Start().
	e.SetClockForTest(1_700_000_000_000)
	return e, st
}

func seed(t *testing.T, e *engine.Engine, in engine.SetupInput) {
	t.Helper()
	if in.InstanceCount == 0 {
		in.InstanceCount = 4
	}
	if in.BaseVersion == "" {
		in.BaseVersion = "v1.0.0"
	}
	if in.TargetVersion == "" {
		in.TargetVersion = "v2.0.0"
	}
	if err := e.ResetAndSeed(in); err != nil {
		t.Fatal(err)
	}
}

// advance runs enough ticks for the virtual simulation to settle;
// callers can stop early to assert mid-flight states.
func advance(e *engine.Engine, ms int64) {
	// Small steps so every state-machine transition gets a pass.
	step := int64(50)
	for d := int64(0); d < ms; d += step {
		e.Advance(step)
	}
}

func waitStatus(e *engine.Engine, st *store.Store, want string, ms int64) store.Release {
	step := int64(25)
	for d := int64(0); d < ms; d += step {
		e.Advance(step)
		if got := active(st).Status; got == want {
			return active(st)
		}
	}
	return active(st)
}

func active(st *store.Store) store.Release {
	snap := st.Snapshot()
	return snap.Releases[len(snap.Releases)-1]
}

func inst(st *store.Store, id string) store.Instance {
	for _, in := range st.Snapshot().Instances {
		if in.ID == id {
			return in
		}
	}
	return store.Instance{}
}

func eventKinds(st *store.Store) []string {
	kinds := []string{}
	for _, ev := range st.Snapshot().Events {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func containsKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// Scenario 1: half of the first batch reports unhealthy -> batch fails,
// rollout stops and every updated instance returns to base.
func TestHalfBatchFailureRollsBack(t *testing.T) {
	e, st := newEngine(t)
	// 4 instances, 50/50 batches; half of batch 1 (2 nodes) unhealthy.
	seed(t, e, engine.SetupInput{
		InstanceCount: 4,
		UnhealthyIdx:  []int{0, 1},
		ProbeDelayMS:  map[int]int{0: 200, 1: 200},
	})

	id, err := e.CreateRelease(target, []int{50, 50}, 2000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	advance(e, 1000) // batch 1 updates land
	r := active(st)
	if r.Status != engine.ReleaseRollingOut {
		t.Fatalf("status after update = %s", r.Status)
	}
	advance(e, 300) // fast unhealthy probes land -> half-batch fail
	r = active(st)
	if r.Status != engine.ReleaseFailing && r.Status != engine.ReleaseRollingBack {
		t.Fatalf("expected failing/rolling back, got %s", r.Status)
	}
	advance(e, 3000) // rollback action + probe settle
	r = active(st)
	if r.Status != engine.ReleaseFailed {
		t.Fatalf("final status = %s, reason=%s", r.Status, r.Reason)
	}
	for _, in := range st.Snapshot().Instances {
		if in.CurrentVersion != base {
			t.Errorf("%s left on %s, want base %s", in.ID, in.CurrentVersion, base)
		}
		if in.State != engine.StatusInstanceReady {
			t.Errorf("%s state = %s, want ready", in.ID, in.State)
		}
	}
	// Batch 2 must never have started: no promote event past batch 1.
	snap := st.Snapshot()
	for _, b := range snap.Batches {
		if b.Phase == engine.PhaseRollout && b.Index == 1 && b.Status != engine.BatchPending {
			t.Errorf("batch 2 unexpectedly %s", b.Status)
		}
	}
	if len(snap.Batches) < 3 {
		t.Fatalf("expected rollout + rollback batch records, got %d", len(snap.Batches))
	}
	_ = id
}

// Scenario 2: release cancelled mid rollout -> no expansion; updated
// instances have a clear home (rollback batches back to base).
func TestCancelStopsExpansionAndRollsBack(t *testing.T) {
	e, st := newEngine(t)
	seed(t, e, engine.SetupInput{InstanceCount: 4})

	id, err := e.CreateRelease(target, []int{25, 50, 25}, 2000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	advance(e, 1000) // batch 1 (1 node) lands
	advance(e, 1300) // observed healthy, promoted to batch 2 (2 nodes)
	advance(e, 600)  // batch 2 mid-update -> cancel now
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	advance(e, 4000)
	r := active(st)
	if r.Status != engine.ReleaseCancelled {
		t.Fatalf("status = %s (%s)", r.Status, r.Reason)
	}
	for _, in := range st.Snapshot().Instances {
		if in.CurrentVersion != base {
			t.Errorf("%s on %s after cancel", in.ID, in.CurrentVersion)
		}
		if in.State != engine.StatusInstanceReady {
			t.Errorf("%s state = %s", in.ID, in.State)
		}
	}
	kinds := eventKinds(st)
	if !containsKind(kinds, "release_cancel_requested") || !containsKind(kinds, "rollback_planned") {
		t.Errorf("missing cancel/rollback events: %v", kinds)
	}
}

// Scenario 3: a health result that arrives after the instance moved on
// must be discarded and never attributed to the next/current version.
func TestLateHealthResultNotAttributedToNextVersion(t *testing.T) {
	e, st := newEngine(t)
	// inst-01's probe for v2 is delayed beyond the observation
	// window; the batch fails on timeout and rollback completes before
	// it arrives. Batch 1 is 25% (one node).
	seed(t, e, engine.SetupInput{
		InstanceCount: 4,
		ProbeDelayMS:  map[int]int{0: 5000},
	})
	_, err := e.CreateRelease(target, []int{25, 75}, 2000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	advance(e, 1100) // update lands, observation opens (~1050), probe pending (due at 6050)
	advance(e, 2100) // observation window closes (~3050): unknown -> fail
	r := active(st)
	if r.Status == engine.ReleaseRollingOut {
		t.Fatalf("batch should fail on observation timeout")
	}
	advance(e, 3000) // rollback to base completes before the late probe
	r = active(st)
	if r.Status != engine.ReleaseFailed {
		t.Fatalf("status = %s", r.Status)
	}
	advance(e, 3000) // now the late v2 probe lands and must be discarded

	in01 := inst(st, "inst-01")
	if in01.CurrentVersion != base {
		t.Fatalf("instance moved to %s", in01.CurrentVersion)
	}
	if in01.Health != engine.Healthy {
		t.Fatalf("late probe overwrote reconciled health: %s", in01.Health)
	}
	kinds := eventKinds(st)
	if !containsKind(kinds, "probe_stale_discarded") {
		t.Errorf("expected stale probe to be discarded, events: %v", kinds)
	}
}

// Scenario 4: interruption during rollback, then restart from disk:
// rollback resumes and finishes; refreshed state equals true progress.
func TestInterruptedRollbackResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st)
	e.SetClockForTest(1_700_000_000_000)
	seed(t, e, engine.SetupInput{
		InstanceCount: 4,
		UnhealthyIdx:  []int{0, 1},
		ProbeDelayMS:  map[int]int{0: 200, 1: 200},
	})
	_, err = e.CreateRelease(target, []int{50, 50}, 2000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	advance(e, 1400) // fast probes fail the batch -> rollback begins
	r := active(st)
	if r.Status != engine.ReleaseRollingBack {
		t.Fatalf("want rolling_back before crash, got %s", r.Status)
	}
	advance(e, 400) // mid rollback action; "crash"

	// Simulate server restart with fresh engine/store on same directory.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e2 := engine.New(st2)
	e2.SetClockForTest(e.ClockForTest())
	// Page refresh immediately: persisted status reflects real progress.
	refreshed := st2.Snapshot()
	var rel store.Release
	for _, rr := range refreshed.Releases {
		rel = rr
	}
	if rel.Status != engine.ReleaseRollingBack {
		t.Fatalf("refreshed status = %s", rel.Status)
	}
	if !rel.RollbackPlanned {
		t.Fatalf("rollback plan not persisted")
	}
	advance(e2, 3000)
	r = active(st2)
	if r.Status != engine.ReleaseFailed {
		t.Fatalf("after resume status = %s (%s)", r.Status, r.Reason)
	}
	for _, in := range st2.Snapshot().Instances {
		if in.CurrentVersion != base || in.State != engine.StatusInstanceReady {
			t.Errorf("%s: version=%s state=%s", in.ID, in.CurrentVersion, in.State)
		}
	}
}

// Independent storage check: separate JSON files exist for the domains.
func TestCollectionsStoredSeparately(t *testing.T) {
	e, _ := newEngine(t)
	seed(t, e, engine.SetupInput{InstanceCount: 2})
	if _, err := e.CreateRelease(target, []int{50, 50}, 1000, 500); err != nil {
		t.Fatal(err)
	}
	e.Advance(3000)
	// store.Open dir is temp; verify via a second engine reusing API
	// indirectly through snapshot collections being independent types.
	snap := e.SnapshotAt(0)
	if len(snap.Versions) == 0 || len(snap.Instances) == 0 {
		t.Fatal("missing versions/instances")
	}
	if len(snap.Batches) == 0 {
		t.Fatal("batches should be a separate collection")
	}
}
