package engine

import (
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
				e.startBatchRollback(tx, rel, b, now, "批次内存在更新失败的实例")
				return true
			}
		}
		// 2. unhealthy observation -> rollback.
		for i := range b.Members {
			if b.Members[i].HealthState == "unhealthy" {
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
			b.Phase = "succeeded"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", batchLabel(b)+" 全部通过健康观察")
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
		rel.Status = "rolled_back"
		rel.Message = "发布失败：失败批次已回退，后续批次未开始"
		tx.AddEvent(nowStr(now), rel.ID, "", "", "error", "发布失败并完成回退")
	}
}

func isReleaseTerminal(s string) bool {
	return s == "succeeded" || s == "rolled_back" || s == "cancelled" || s == "failed"
}

func isBatchTerminal(s string) bool {
	return s == "succeeded" || s == "rolled_back" || s == "cancelled" || s == "failed"
}
