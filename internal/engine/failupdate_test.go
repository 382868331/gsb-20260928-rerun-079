package engine_test

import (
	"testing"

	"releaselab/internal/engine"
)

// A failed update keeps the instance on the base version; the batch
// fails immediately and updated peers are rolled back.
func TestUpdateFailureRollsBack(t *testing.T) {
	e, st := newEngine(t)
	seed(t, e, engine.SetupInput{InstanceCount: 4, FailUpdateIdx: []int{0}})
	_, err := e.CreateRelease(target, []int{50, 50}, 2000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	advance(e, 1100) // update phase: inst-01 fails, inst-02 succeeds
	r := active(st)
	if r.Status != engine.ReleaseFailing && r.Status != engine.ReleaseRollingBack {
		t.Fatalf("status = %s (%s)", r.Status, r.Reason)
	}
	advance(e, 3000)
	r = active(st)
	if r.Status != engine.ReleaseFailed {
		t.Fatalf("final = %s (%s)", r.Status, r.Reason)
	}
	for _, in := range st.Snapshot().Instances {
		if in.CurrentVersion != base {
			t.Errorf("%s on %s", in.ID, in.CurrentVersion)
		}
		if in.State != engine.StatusInstanceReady {
			t.Errorf("%s state = %s", in.ID, in.State)
		}
	}
}
