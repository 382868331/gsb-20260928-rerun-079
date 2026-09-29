package engine

import (
	"fmt"

	"releaselab/internal/store"
)

// CreateRelease splits the fleet into batches (by percentage points)
// and persists the release plus all batch records up front. Batches are
// separate records from versions and instances.
func (e *Engine) CreateRelease(targetVersionID string, percents []int, healthWaitMS, actionMS int64) (string, error) {
	now := e.clock()
	id := "rel-" + fmt.Sprintf("%06d-%s", now%1e6, token()[:6])
	err := e.st.Mutate(func(tx *store.Tx) error {
		if activeRelease(tx) != nil {
			return fmt.Errorf("another release is already in progress")
		}
		var target *store.Version
		for i := range tx.Versions() {
			if tx.Versions()[i].ID == targetVersionID {
				target = &tx.Versions()[i]
			}
		}
		if target == nil {
			return fmt.Errorf("version %q not found", targetVersionID)
		}
		insts := tx.Instances()
		if len(insts) == 0 {
			return fmt.Errorf("no instances to release to")
		}
		base := insts[0].CurrentVersion
		for _, in := range insts {
			if in.State != StatusInstanceReady {
				return fmt.Errorf("instance %s is not ready", in.ID)
			}
			if in.CurrentVersion != base {
				return fmt.Errorf("instances run mixed versions; stabilize the fleet first")
			}
		}
		if base == targetVersionID {
			return fmt.Errorf("fleet already on target version")
		}
		if len(percents) == 0 {
			percents = []int{25, 50, 100}
		}
		total := 0
		for _, p := range percents {
			total += p
		}
		if total != 100 {
			return fmt.Errorf("batch percents must sum to 100, got %d", total)
		}

		groups := splitBatches(insts, percents)
		r := store.Release{
			ID:               id,
			TargetVersionID:  targetVersionID,
			BaseVersionID:    base,
			Strategy:         "batch",
			BatchPercent:     append([]int(nil), percents...),
			Status:           ReleasePending,
			CurrentBatchIdx:  0,
			HealthWaitMS:     int(healthWaitMS),
			ActionDurationMS: int(actionMS),
			CreatedAtMS:      now,
			UpdatedAtMS:      now,
		}
		tx.AddRelease(r)
		for idx, group := range groups {
			ids := make([]string, 0, len(group))
			for _, id := range group {
				ids = append(ids, id)
			}
			tx.AddBatch(store.Batch{
				ID:           fmt.Sprintf("%s-b%d", id, idx+1),
				ReleaseID:    id,
				Index:        idx,
				Phase:        PhaseRollout,
				Status:       BatchPending,
				InstanceIDs:  ids,
				PendingCount: len(ids),
			})
		}
		e.log(tx, now, "release_created",
			fmt.Sprintf("Release %s created: %s -> %s in %d batch(es)", id, base, targetVersionID, len(groups)),
			id, "", "")
		return nil
	})
	return id, err
}

func splitBatches(insts []store.Instance, percents []int) [][]string {
	n := len(insts)
	groups := make([][]string, 0, len(percents))
	start := 0
	assigned := 0
	for i, p := range percents {
		end := n
		if i < len(percents)-1 {
			end = n * (assigned + p) / 100
		}
		group := make([]string, 0, end-start)
		for j := start; j < end; j++ {
			group = append(group, insts[j].ID)
		}
		groups = append(groups, group)
		start = end
		assigned += p
	}
	return groups
}

// Cancel marks the release cancelled. No new batch is ever expanded;
// if instances were already updated they are rolled back to base.
func (e *Engine) Cancel(releaseID string) error {
	now := e.clock()
	return e.st.Mutate(func(tx *store.Tx) error {
		r := tx.Release(releaseID)
		if r == nil {
			return fmt.Errorf("release %q not found", releaseID)
		}
		switch r.Status {
		case ReleaseSucceeded, ReleaseFailed, ReleaseCancelled:
			return fmt.Errorf("release already finished")
		}
		r.CancelRequested = true
		switch r.Status {
		case ReleasePending, ReleasePaused, ReleaseRollingOut:
			r.Status = ReleaseFailing
			r.Reason = "cancelled by operator"
		}
		r.UpdatedAtMS = now
		tx.SaveRelease(*r)
		e.log(tx, now, "release_cancel_requested",
			"Cancel requested: no further batches will expand; updated instances roll back to base",
			releaseID, "", "")
		return nil
	})
}

func (e *Engine) advanceRollout(tx *store.Tx, r *store.Release, now int64) {
	b := currentRolloutBatch(tx, r)
	if b == nil {
		// All batches healthy -> release succeeds.
		r.Status = ReleaseSucceeded
		r.FinishedAtMS = now
		r.UpdatedAtMS = now
		tx.SaveRelease(*r)
		e.log(tx, now, "release_succeeded",
			fmt.Sprintf("Release %s succeeded: fleet is on %s", r.ID, r.TargetVersionID), r.ID, "", "")
		return
	}

	switch b.Status {
	case BatchPending:
		if r.Status == ReleasePending {
			r.Status = ReleaseRollingOut
			r.UpdatedAtMS = now
			tx.SaveRelease(*r)
		}
		e.startBatchUpdate(tx, r, b, now)
	case BatchUpdating:
		// actions complete via completeDueActions; once all finished
		// (or failed) move to observing / failure.
		if batchActionsDone(tx, b) {
			if len(b.UpdateFailedIDs) > 0 {
				e.failBatch(tx, r, b, now, fmt.Sprintf("%d instance(s) failed to update", len(b.UpdateFailedIDs)))
				return
			}
			b.Status = BatchObserving
			b.StartedAtMS = now
			b.DeadlineMS = now + int64(r.HealthWaitMS)
			tx.SaveBatch(*b)
			for _, in := range sortedInstances(tx, b.InstanceIDs) {
				in.State = StatusInstanceObserving
				in.Health = HealthUnknown
				in.UpdatedAtMS = now
				tx.SaveInstance(*in)
			}
			e.log(tx, now, "batch_observing",
				fmt.Sprintf("Batch %d updated; health observation window open (%dms)", b.Index+1, r.HealthWaitMS),
				r.ID, b.ID, "")
		}
	case BatchObserving:
		e.evaluateObservation(tx, r, b, now)
	case BatchHealthy:
		// Cancel may arrive just after a batch went healthy: do not expand.
		if r.CancelRequested {
			r.Status = ReleaseFailing
			r.Reason = "cancelled by operator"
			r.UpdatedAtMS = now
			tx.SaveRelease(*r)
			return
		}
		r.CurrentBatchIdx = b.Index + 1
		r.UpdatedAtMS = now
		tx.SaveRelease(*r)
		e.log(tx, now, "batch_promote",
			fmt.Sprintf("Batch %d healthy; expanding to batch %d", b.Index+1, b.Index+2),
			r.ID, b.ID, "")
	}
}

func currentRolloutBatch(tx *store.Tx, r *store.Release) *store.Batch {
	for i := range tx.Batches() {
		b := &tx.Batches()[i]
		if b.ReleaseID == r.ID && b.Phase == PhaseRollout && b.Index == r.CurrentBatchIdx {
			return b
		}
	}
	return nil
}

func (e *Engine) startBatchUpdate(tx *store.Tx, r *store.Release, b *store.Batch, now int64) {
	b.Status = BatchUpdating
	b.StartedAtMS = now
	tx.SaveBatch(*b)
	for _, in := range sortedInstances(tx, b.InstanceIDs) {
		tk := token()
		in.State = StatusInstanceUpdating
		in.Health = HealthUnknown
		in.ReleaseID = r.ID
		in.BatchID = b.ID
		in.Action = ActionUpdate
		in.ActionVersion = r.TargetVersionID
		in.GenerationToken = tk
		in.DesiredVersion = r.TargetVersionID
		in.ActionStartMS = now
		in.UpdatedAtMS = now
		tx.SaveInstance(*in)
		e.log(tx, now, "instance_update_start",
			fmt.Sprintf("%s: updating %s -> %s", in.Name, in.CurrentVersion, r.TargetVersionID),
			r.ID, b.ID, in.ID)
	}
	e.log(tx, now, "batch_update_start",
		fmt.Sprintf("Batch %d updating %d instance(s)", b.Index+1, len(b.InstanceIDs)), r.ID, b.ID, "")
}

func batchActionsDone(tx *store.Tx, b *store.Batch) bool {
	for _, in := range sortedInstances(tx, b.InstanceIDs) {
		if in.State == StatusInstanceUpdating || in.State == StatusInstanceRollingBack {
			return false
		}
	}
	return true
}

func (e *Engine) failBatch(tx *store.Tx, r *store.Release, b *store.Batch, now int64, reason string) {
	b.Status = BatchFailed
	b.FinishedAtMS = now
	tx.SaveBatch(*b)
	r.Status = ReleaseFailing
	r.Reason = reason
	r.UpdatedAtMS = now
	tx.SaveRelease(*r)
	e.log(tx, now, "batch_failed",
		fmt.Sprintf("Batch %d failed: %s — rolling updated instances back to %s", b.Index+1, reason, r.BaseVersionID),
		r.ID, b.ID, "")
}

func (e *Engine) evaluateObservation(tx *store.Tx, r *store.Release, b *store.Batch, now int64) {
	half := (len(b.InstanceIDs) + 1) / 2
	// Half-batch drill: unhealthy reaching at least half fails fast.
	if b.UnhealthyCount >= half {
		e.failBatch(tx, r, b, now, fmt.Sprintf("%d of %d instance(s) reported unhealthy (half-batch threshold)", b.UnhealthyCount, len(b.InstanceIDs)))
		return
	}
	if now >= b.DeadlineMS {
		if b.PendingCount > 0 {
			// Observation window elapsed without a verdict from some
			// instances: treat unknown as failure for safety.
			pending := b.PendingCount
			e.failBatch(tx, r, b, now, fmt.Sprintf("health unknown for %d instance(s) after observation window", pending))
			return
		}
		b.Status = BatchHealthy
		b.FinishedAtMS = now
		tx.SaveBatch(*b)
		for _, in := range sortedInstances(tx, b.InstanceIDs) {
			in.Health = Healthy
			in.State = StatusInstanceReady
			in.ReleaseID = r.ID
			in.BatchID = b.ID
			in.Action = ""
			in.GenerationToken = ""
			in.CurrentVersion = r.TargetVersionID
			in.DesiredVersion = ""
			in.UpdatedAtMS = now
			tx.SaveInstance(*in)
		}
		e.log(tx, now, "batch_healthy",
			fmt.Sprintf("Batch %d passed health observation", b.Index+1), r.ID, b.ID, "")
	}
}
