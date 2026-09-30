package engine

import (
	"fmt"
	"time"

	"releasectl/internal/store"
)

// reconcile performs one pass of state transitions driven by member states.
func (e *Engine) reconcile(tx *store.Tx, now time.Time) (bool, error) {
	for i := range tx.Releases() {
		rel := &tx.Releases()[i]
		if isReleaseTerminal(rel.Status) {
			continue
		}
		batches := releaseBatches(tx, rel.ID)
		activeIdx := -1
		for j := range batches {
			if isBatchTerminal(batches[j].Phase) {
				if batches[j].Phase != "succeeded" {
					// Failed/cancelled/rolled-back batch blocks all later
					// batches: they must never expand and stay pending.
					activeIdx = -2
					break
				}
				continue
			}
			activeIdx = j
			break
		}
		if activeIdx == -2 {
			// Release should be finished with the rollback outcome.
			e.finishRelease(tx, rel, batches, now)
			return true, nil
		}
		if activeIdx < 0 {
			e.finishRelease(tx, rel, batches, now)
			return true, nil
		}
		b := batches[activeIdx]
		if rel.Status == "pending" {
			rel.Status = "in_progress"
		}
		changed := e.reconcileBatch(tx, rel, b, now)
		// releaseBatches returns pointers into the tx slice; write back
		for k := range tx.Batches() {
			if tx.Batches()[k].ID == b.ID {
				tx.Batches()[k] = *b
			}
		}
		if changed {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) reconcileBatch(tx *store.Tx, rel *store.Release, b *store.Batch, now time.Time) bool {
	switch b.Phase {
	case "pending":
		// Cancellation before the batch ever started: no instance moved, the
		// batch is simply skipped (never expands).
		if rel.CancelRequested {
			b.Phase = "cancelled"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", "发布已取消，未开始的批次不再扩批")
			return true
		}
		b.Phase = "updating"
		b.StartedAt = nowStr(now)
		for i := range b.Members {
			m := &b.Members[i]
			m.UpdateState = "in_progress"
			tx.SetOps(append(tx.Ops(), store.Op{
				ID: newID(tx, "op"), Kind: "update", ReleaseID: rel.ID, BatchID: b.ID,
				InstanceID: m.InstanceID, FromVersion: m.FromVersion, ToVersion: m.ToVersion,
				DueAt: now.Add(e.c.UpdateDelay).Format(time.RFC3339Nano),
			}))
		}
		tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", batchLabel(b)+" 开始扩批更新")
		return true

	case "updating", "observing":
		// 1. update failure -> rollback the whole batch.
		for i := range b.Members {
			if b.Members[i].UpdateState == "failed" {
				if rel.Failure.PauseOnFailure && !rel.ResumeRequested && !rel.CancelRequested {
					e.pauseBatch(tx, rel, b, now, "failure", "存在更新失败的实例")
					return true
				}
				e.startBatchRollback(tx, rel, b, now, "批次内存在更新失败的实例")
				return true
			}
		}
		// 2. unhealthy observation -> rollback.
		for i := range b.Members {
			if b.Members[i].HealthState == "unhealthy" {
				if rel.Failure.PauseOnFailure && !rel.ResumeRequested && !rel.CancelRequested {
					e.pauseBatch(tx, rel, b, now, "failure", "健康观察未通过")
					return true
				}
				e.startBatchRollback(tx, rel, b, now, "健康观察未通过")
				return true
			}
		}
		// 3. cancel while the batch is running: stop expanding, but every
		// already-updated instance must have a clear destination, so roll the
		// whole active batch back to its from-version.
		if rel.CancelRequested && rel.Status != "cancelling" {
			rel.Status = "cancelling"
		}
		if rel.CancelRequested {
			allDone := true
			for i := range b.Members {
				m := &b.Members[i]
				if m.UpdateState == "pending" || m.UpdateState == "in_progress" {
					allDone = false
				}
			}
			if allDone {
				e.startBatchRollback(tx, rel, b, now, "发布已取消，当前批次整体回退，已更新实例返回原版本")
				return true
			}
		}
		// 4. everyone healthy -> batch done, release continues to next batch.
		allHealthy := true
		for i := range b.Members {
			if b.Members[i].HealthState != "healthy" {
				allHealthy = false
			}
		}
		if allHealthy {
			// Manual review pause: the batch's health evidence is complete,
			// freeze here so the operator can inspect it before expanding.
			if rel.ReviewRequested {
				rel.ReviewRequested = false
				e.pauseBatch(tx, rel, b, now, "review", "健康证据已收集完成，等待人工审查")
				return true
			}
			b.Phase = "succeeded"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", batchLabel(b)+" 全部通过健康观察")
			return true
		}
		return false

	case "paused":
		// Paused for manual review. Two flavours:
		// - failure review: resume the rollback or end the drill keeping the
		//   live version distribution;
		// - health-evidence review: continue, shrink remaining batches, or
		//   roll back everything that moved.
		switch {
		case rel.RollbackAllRequested:
			rel.RollbackAllRequested = false
			e.startReleaseRollback(tx, rel, now, "审查完成，回退全部已更新实例")
			return true
		case rel.ContinueRequested:
			rel.ContinueRequested = false
			if rel.ShrinkTo > 0 {
				e.shrinkPendingBatches(tx, rel, b, rel.ShrinkTo, now)
				rel.ShrinkTo = 0
			}
			b.Phase = "succeeded"
			b.FinishedAt = nowStr(now)
			rel.Status = "in_progress"
			rel.Message = ""
			rel.PauseKind = ""
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", batchLabel(b)+" 审查通过，继续发布")
			return true
		case rel.ResumeRequested || rel.CancelRequested:
			rel.ResumeRequested = false
			e.startBatchRollback(tx, rel, b, now, "审查完成，继续批次回退")
			return true
		case rel.FinishRequested:
			b.Phase = "failed"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "error", "审查后结束本次演练：保留当前版本分布，不执行回退")
			return true
		}
		return false

	case "rolling_back":
		for i := range b.Members {
			if b.Members[i].UpdateState == "rollback_in_progress" {
				return false
			}
		}
		if rel.CancelRequested {
			b.Phase = "cancelled"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", batchLabel(b)+" 已完成回退并停止继续发布")
		} else {
			b.Phase = "rolled_back"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "error", batchLabel(b)+" 已完成回退")
		}
		return true
	}
	return false
}

func (e *Engine) startBatchRollback(tx *store.Tx, rel *store.Release, b *store.Batch, now time.Time, reason string) {
	b.Phase = "rolling_back"
	rel.Status = "rolling_back"
	for i := range b.Members {
		m := &b.Members[i]
		if m.UpdateState == "rolled_back" || m.UpdateState == "rollback_in_progress" {
			continue
		}
		if m.UpdateState == "succeeded" {
			// Instance really moved: roll it back explicitly.
			m.UpdateState = "rollback_in_progress"
			m.Message = "正在回退到原版本 " + m.FromVersion
			tx.SetOps(append(tx.Ops(), store.Op{
				ID: newID(tx, "op"), Kind: "rollback", ReleaseID: rel.ID, BatchID: b.ID,
				InstanceID: m.InstanceID, FromVersion: m.FromVersion, ToVersion: m.ToVersion,
				DueAt: now.Add(e.c.RollbackDelay).Format(time.RFC3339Nano),
			}))
		} else {
			// Never updated (pending/in_progress/failed): its destination is
			// the version it never left; mark that explicitly.
			m.UpdateState = "rolled_back"
			if m.HealthState == "pending" || m.HealthState == "in_progress" {
				m.HealthState = "stale"
			}
			m.Message = "未完成更新，归宿仍为原版本 " + m.FromVersion
		}
	}
	tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", batchLabel(b)+" 开始回退："+reason)
}

// pauseBatch freezes the batch the moment a member fails so an operator can
// inspect which instances are on the new version and which stayed put. No
// rollback op is scheduled until the rollback is explicitly resumed.
func (e *Engine) pauseBatch(tx *store.Tx, rel *store.Release, b *store.Batch, now time.Time, kind, reason string) {
	b.Phase = "paused"
	rel.Status = "paused"
	rel.PauseKind = kind
	if kind == "review" {
		rel.Message = "已暂停：健康证据已就绪，审查后可继续、缩小批次或回退"
	} else {
		rel.Message = "已暂停：可审查失败实例，再选择继续回退或结束本次演练"
	}
	tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", batchLabel(b)+" 暂停："+reason)
}

// shrinkPendingBatches rebuilds the release's pending batches with a smaller
// batch size. Only batches that never started are touched; their members
// never left the baseline, so re-slicing them cannot re-execute any work.
func (e *Engine) shrinkPendingBatches(tx *store.Tx, rel *store.Release, current *store.Batch, size int, now time.Time) {
	var pending []*store.Batch
	for i := range tx.Batches() {
		b := &tx.Batches()[i]
		if b.ReleaseID == rel.ID && b.Phase == "pending" {
			pending = append(pending, b)
		}
	}
	if len(pending) == 0 {
		return
	}
	var members []store.MemberState
	for _, b := range pending {
		members = append(members, b.Members...)
	}
	pendingIDs := map[string]bool{}
	for _, b := range pending {
		pendingIDs[b.ID] = true
	}
	// Build a fresh slice: the caller may hold pointers into the old backing
	// array (the currently reconciled batch), so never filter in place.
	kept := make([]store.Batch, 0, len(tx.Batches()))
	for i := range tx.Batches() {
		if !pendingIDs[tx.Batches()[i].ID] {
			kept = append(kept, tx.Batches()[i])
		}
	}
	tx.SetBatches(kept)

	idx := current.Index + 1
	rel.BatchIDs = rel.BatchIDs[:idx]
	for start := 0; start < len(members); start += size {
		end := start + size
		if end > len(members) {
			end = len(members)
		}
		nb := store.Batch{
			ID: newID(tx, "batch"), ReleaseID: rel.ID, Index: idx, Phase: "pending",
			TargetVersion: rel.TargetVersion,
		}
		for _, m := range members[start:end] {
			m.UpdateState = "pending"
			m.HealthState = "pending"
			m.Message = ""
			m.LastResolvedKey = ""
			nb.Members = append(nb.Members, m)
		}
		tx.SetBatches(append(tx.Batches(), nb))
		rel.BatchIDs = append(rel.BatchIDs, nb.ID)
		idx++
	}
	rel.BatchSize = size
	tx.AddEvent(nowStr(now), rel.ID, "", "", "info",
		fmt.Sprintf("后续批次已按每批 %d 台重新划分，已完成批次不受影响", size))
}

// startReleaseRollback rolls back every instance that actually moved across
// all batches of the release (including already succeeded batches), and
// cancels batches that never started.
func (e *Engine) startReleaseRollback(tx *store.Tx, rel *store.Release, now time.Time, reason string) {
	rel.Status = "rolling_back"
	rel.Message = ""
	rel.PauseKind = ""
	for i := range tx.Batches() {
		b := &tx.Batches()[i]
		if b.ReleaseID != rel.ID {
			continue
		}
		switch b.Phase {
		case "pending":
			b.Phase = "cancelled"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", batchLabel(b)+" 未开始，随整体回退取消")
		case "succeeded", "paused":
			e.startBatchRollback(tx, rel, b, now, reason)
		}
	}
}

func (e *Engine) finishRelease(tx *store.Tx, rel *store.Release, batches []*store.Batch, now time.Time) {
	rel.FinishedAt = nowStr(now)
	allOK := true
	for _, b := range batches {
		if b.Phase != "succeeded" {
			allOK = false
		}
	}
	switch {
	case allOK:
		rel.Status = "succeeded"
		rel.Message = "全部批次发布成功"
		for i := range tx.Instances() {
			inst := &tx.Instances()[i]
			if inAnyBatch(batches, inst.ID) {
				inst.DesiredVersion = rel.TargetVersion
			}
		}
		tx.AddEvent(nowStr(now), rel.ID, "", "", "info", "发布成功：所有实例已运行 "+rel.TargetVersion)
	case rel.CancelRequested:
		for _, b := range batches {
			if b.Phase == "pending" {
				b.Phase = "cancelled"
				b.FinishedAt = nowStr(now)
			}
		}
		rel.Status = "cancelled"
		rel.Message = "已取消：后续批次未扩批，已更新实例全部回退"
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "发布已取消，实例归宿均为原版本")
	default:
		endedAfterReview := false
		for _, b := range batches {
			if b.Phase == "failed" {
				endedAfterReview = true
			}
		}
		if endedAfterReview {
			// Operator ended the drill without rollback: untouched batches are
			// marked cancelled (they never expanded), live versions are kept.
			for _, b := range batches {
				if b.Phase == "pending" {
					b.Phase = "cancelled"
					b.FinishedAt = nowStr(now)
				}
			}
			rel.Status = "failed"
			rel.Message = "审查后结束：保留当前真实版本分布，未继续回退，后续批次未扩批"
			tx.AddEvent(nowStr(now), rel.ID, "", "", "error", "演练已结束，实例版本分布保持暂停时的真实状态")
		} else {
			rel.Status = "rolled_back"
			rel.Message = "发布失败：失败批次已回退，后续批次未开始"
			tx.AddEvent(nowStr(now), rel.ID, "", "", "error", "发布失败并完成回退")
		}
	}
}

func isReleaseTerminal(s string) bool {
	return s == "succeeded" || s == "rolled_back" || s == "cancelled" || s == "failed"
}

func isBatchTerminal(s string) bool {
	return s == "succeeded" || s == "rolled_back" || s == "cancelled" || s == "failed"
}
