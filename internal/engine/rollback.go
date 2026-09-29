package engine

import (
	"fmt"
	"sort"

	"releaselab/internal/store"
)

// planAndStartRollback creates rollback batches (one per rollout batch
// that touched instances), then begins the last one first. Planning is
// idempotent and persisted, so a restart mid-rollback resumes instead
// of double-planning.
func (e *Engine) planAndStartRollback(tx *store.Tx, r *store.Release, now int64) {
	// Wait for in-flight update actions to settle so every updated
	// instance has a clear destination.
	for i := range tx.Instances() {
		in := &tx.Instances()[i]
		if in.ReleaseID == r.ID && (in.State == StatusInstanceUpdating) {
			return
		}
	}

	if !r.RollbackPlanned {
		e.planRollback(tx, r, now)
	}

	rbs := rollbackBatches(tx, r.ID)
	if len(rbs) == 0 {
		e.finishRollback(tx, r, now)
		return
	}
	idx := r.RollbackBatchIdx
	if idx < 0 {
		idx = 0
	}
	if idx >= len(rbs) {
		e.finishRollback(tx, r, now)
		return
	}
	r.Status = ReleaseRollingBack
	r.UpdatedAtMS = now
	tx.SaveRelease(*r)
	b := rbs[idx]
	e.advanceOneRollbackBatch(tx, r, b, now)
}

func (e *Engine) planRollback(tx *store.Tx, r *store.Release, now int64) {
	// Instances with a clear home to reconcile: those currently running
	// the failed target version, plus any stuck mid-action on it.
	home := map[string]bool{}
	for i := range tx.Instances() {
		in := &tx.Instances()[i]
		if in.ReleaseID != r.ID {
			continue
		}
		if in.CurrentVersion == r.TargetVersionID || in.ActionVersion == r.TargetVersionID {
			home[in.ID] = true
		}
	}
	// Build rollback batches in reverse rollout order: latest wave first.
	rollout := rolloutBatches(tx, r.ID)
	sort.Slice(rollout, func(a, b int) bool { return rollout[a].Index > rollout[b].Index })
	n := 0
	for _, ob := range rollout {
		ids := []string{}
		for _, id := range ob.InstanceIDs {
			if home[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		tx.AddBatch(store.Batch{
			ID:           fmt.Sprintf("%s-r%d", r.ID, ob.Index+1),
			ReleaseID:    r.ID,
			Index:        n,
			Phase:        PhaseRollback,
			Status:       BatchPending,
			InstanceIDs:  ids,
			PendingCount: len(ids),
		})
		n++
	}
	r.RollbackPlanned = true
	r.RollbackBatchIdx = 0
	r.UpdatedAtMS = now
	tx.SaveRelease(*r)
	if n == 0 {
		e.log(tx, now, "rollback_nothing",
			"No instance reached the failed version; fleet already at base version", r.ID, "", "")
	} else {
		e.log(tx, now, "rollback_planned",
			fmt.Sprintf("Rollback planned: %d batch(es) back to %s, newest wave first", n, r.BaseVersionID),
			r.ID, "", "")
	}
}

func rolloutBatches(tx *store.Tx, releaseID string) []store.Batch {
	out := []store.Batch{}
	for _, b := range tx.Batches() {
		if b.ReleaseID == releaseID && b.Phase == PhaseRollout {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func rollbackBatches(tx *store.Tx, releaseID string) []*store.Batch {
	out := []*store.Batch{}
	for i := range tx.Batches() {
		b := &tx.Batches()[i]
		if b.ReleaseID == releaseID && b.Phase == PhaseRollback {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func (e *Engine) advanceRollback(tx *store.Tx, r *store.Release, now int64) {
	rbs := rollbackBatches(tx, r.ID)
	idx := r.RollbackBatchIdx
	if idx >= len(rbs) {
		e.finishRollback(tx, r, now)
		return
	}
	e.advanceOneRollbackBatch(tx, r, rbs[idx], now)
}

func (e *Engine) advanceOneRollbackBatch(tx *store.Tx, r *store.Release, b *store.Batch, now int64) {
	switch b.Status {
	case BatchPending:
		b.Status = BatchRollingBack
		b.StartedAtMS = now
		b.DeadlineMS = 0
		tx.SaveBatch(*b)
		for _, in := range sortedInstances(tx, b.InstanceIDs) {
			if in.CurrentVersion == r.BaseVersionID && in.State == StatusInstanceReady {
				continue
			}
			tk := token()
			in.State = StatusInstanceRollingBack
			in.Health = HealthUnknown
			in.ReleaseID = r.ID
			in.BatchID = b.ID
			in.Action = ActionRollback
			in.ActionVersion = r.BaseVersionID
			in.GenerationToken = tk
			in.DesiredVersion = r.BaseVersionID
			in.ActionStartMS = now
			in.UpdatedAtMS = now
			tx.SaveInstance(*in)
			e.log(tx, now, "instance_rollback_start",
				fmt.Sprintf("%s: rolling back %s -> %s", in.Name, in.CurrentVersion, r.BaseVersionID),
				r.ID, b.ID, in.ID)
		}
		e.log(tx, now, "rollback_batch_start",
			fmt.Sprintf("Rollback batch %d: %d instance(s) returning to %s", b.Index+1, len(b.InstanceIDs), r.BaseVersionID),
			r.ID, b.ID, "")

	case BatchRollingBack:
		busy := false
		for _, in := range sortedInstances(tx, b.InstanceIDs) {
			if in.State == StatusInstanceRollingBack {
				busy = true
			}
		}
		if busy {
			return
		}
		// All actions/probes for this wave settled.
		failed := 0
		for _, in := range sortedInstances(tx, b.InstanceIDs) {
			if in.CurrentVersion != r.BaseVersionID {
				failed++
				continue
			}
			in.State = StatusInstanceReady
			in.Health = Healthy
			in.ReleaseID = r.ID
			in.BatchID = b.ID
			in.Action = ""
			in.GenerationToken = ""
			in.DesiredVersion = ""
			in.UpdatedAtMS = now
			tx.SaveInstance(*in)
		}
		b.Status = BatchRolledBack
		b.FinishedAtMS = now
		tx.SaveBatch(*b)
		if failed > 0 {
			e.log(tx, now, "rollback_batch_incomplete",
				fmt.Sprintf("Rollback batch %d: %d instance(s) did not return to base", b.Index+1, failed),
				r.ID, b.ID, "")
		}
		e.log(tx, now, "rollback_batch_done",
			fmt.Sprintf("Rollback batch %d complete", b.Index+1), r.ID, b.ID, "")
		r.RollbackBatchIdx = b.Index + 1
		r.UpdatedAtMS = now
		tx.SaveRelease(*r)
	}
}

func (e *Engine) finishRollback(tx *store.Tx, r *store.Release, now int64) {
	// Reconcile all instances touched by the release that never moved:
	// update-failed machines were always on base but left marked failed.
	stranded := 0
	for i := range tx.Instances() {
		in := &tx.Instances()[i]
		if in.ReleaseID != r.ID {
			continue
		}
		switch {
		case in.CurrentVersion == r.BaseVersionID:
			in.State = StatusInstanceReady
			in.Health = Healthy
			in.Action = ""
			in.GenerationToken = ""
			in.DesiredVersion = ""
			tx.SaveInstance(*in)
		case in.CurrentVersion == r.TargetVersionID:
			stranded++
		}
	}
	if stranded > 0 {
		r.Status = ReleaseFailed
		r.Reason = fmt.Sprintf("rollback incomplete: %d instance(s) stranded on target version", stranded)
	} else if r.CancelRequested {
		r.Status = ReleaseCancelled
		r.Reason = "cancelled; updated instances rolled back to base"
	} else {
		r.Status = ReleaseFailed
		r.Reason = "rolled back to base after batch failure"
	}
	r.UpdatedAtMS = now
	r.FinishedAtMS = now
	tx.SaveRelease(*r)
	e.log(tx, now, "release_"+r.Status,
		fmt.Sprintf("Release %s finished: %s", r.ID, r.Reason), r.ID, "", "")
}
