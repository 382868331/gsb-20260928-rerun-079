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
		// While a review pause is open, pending batches must not start even if
		// the active batch is already frozen on an earlier index.
		if b.Phase == "pending" && rel.Status == "paused" && rel.ReviewAction == "" && !rel.CancelRequested {
			continue
		}
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
		// Compatibility + health basis is evaluated and persisted before the
		// batch takes effect. A blocking constraint (a dependent must update
		// first) freezes the release; a pause-mode constraint freezes it for
		// review exactly once, until a human accepts the evidence.
		// Capacity is evaluated together with them: the persisted preflight
		// shows version compatibility, health basis and the capacity this
		// batch will occupy in one place.
		capOK, capBasis, capBlockers := checkBatchCapacity(tx, rel, b)
		pre := &store.Preflight{
			CheckedAt:     nowStr(now),
			Findings:      evaluateCompat(tx, rel.ServiceID, rel.TargetVersion),
			HealthBasis:   healthBasis(tx, b),
			CapacityBasis: capBasis,
		}
		pre.Compatible = !hasBlockingFinding(pre.Findings) && !hasPausingFinding(pre.Findings)
		if hasBlockingFinding(pre.Findings) {
			b.Preflight = pre
			rel.Status = "paused"
			rel.PauseRequested = true
			rel.CompatBlocked = true
			rel.Message = "兼容约束未满足：依赖者必须先更新，批次暂停推进"
			for _, f := range pre.Findings {
				if f.Blocking && !f.Compatible {
					tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "error", "兼容约束阻止扩批："+f.Detail)
				}
			}
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", batchLabel(b)+" 暂停推进：等待依赖者先更新，约束满足后在审查中选择继续")
			return true
		}
		if hasPausingFinding(pre.Findings) && rel.ReviewAction == "" {
			b.Preflight = pre
			rel.Status = "paused"
			rel.PauseRequested = true
			rel.Message = "兼容审查暂停：依赖者兼容性需人工确认后才能扩批"
			for _, f := range pre.Findings {
				if !f.Compatible {
					tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", "兼容审查："+f.Detail)
				}
			}
			return true
		}
		// Capacity admission: two individually safe batches may still
		// overlap, so the check runs inside the same transaction that
		// starts the batch — the second one always sees the first one's
		// commitments and must wait instead of breaching a floor.
		if !capOK {
			note := ""
			for i, bl := range capBlockers {
				if i > 0 {
					note += "；"
				}
				note += bl
			}
			if !rel.CapacityBlocked || rel.CapacityNote != note {
				rel.CapacityBlocked = true
				rel.CapacityNote = note
				rel.Message = "容量不足，等待其他批次释放承诺：" + note
				b.Preflight = pre
				tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", batchLabel(b)+" 暂不扩批："+note)
				return true
			}
			return false
		}
		if rel.CapacityBlocked {
			rel.CapacityBlocked = false
			rel.CapacityNote = ""
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", "容量承诺已释放，"+batchLabel(b)+" 继续扩批")
		}
		b.Preflight = pre
		for _, f := range pre.Findings {
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", "扩批前兼容检查："+f.Detail)
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
		// A manual review pause freezes every automatic decision: no failure
		// rollback, no cancel rollback, no expansion, while evidence already
		// scheduled (health answers) keeps arriving and is recorded.
		if rel.PauseRequested && rel.Status == "paused" && !rel.CancelRequested && rel.ReviewAction == "" {
			return false
		}
		// 1. update failure -> rollback the whole batch.
		for i := range b.Members {
			if b.Members[i].UpdateState == "failed" {
				if rel.Failure.PauseOnFailure && !rel.ResumeRequested && !rel.CancelRequested && rel.ReviewAction == "" {
					e.pauseBatch(tx, rel, b, now, "存在更新失败的实例")
					return true
				}
				e.startBatchRollback(tx, rel, b, now, "批次内存在更新失败的实例")
				return true
			}
		}
		// 2. unhealthy observation -> rollback.
		for i := range b.Members {
			if b.Members[i].HealthState == "unhealthy" {
				if rel.Failure.PauseOnFailure && !rel.ResumeRequested && !rel.CancelRequested && rel.ReviewAction == "" {
					e.pauseBatch(tx, rel, b, now, "健康观察未通过")
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
			// A healthy batch is not enough: re-check cross-service
			// compatibility before letting the batch take effect, so this
			// release never breaks other services on health evidence alone.
			findings := evaluateCompat(tx, rel.ServiceID, rel.TargetVersion)
			b.Preflight = &store.Preflight{
				CheckedAt:   nowStr(now),
				Compatible:  !hasBlockingFinding(findings),
				Findings:    findings,
				HealthBasis: healthBasis(tx, b),
			}
			if hasBlockingFinding(findings) {
				rel.Status = "paused"
				rel.PauseRequested = true
				rel.CompatBlocked = true
				rel.Message = "批次健康但兼容约束未满足：依赖者必须先更新，批次暂不生效"
				for _, f := range findings {
					if f.Blocking && !f.Compatible {
						tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "error", "批次健康但兼容约束未满足："+f.Detail)
					}
				}
				return true
			}
			b.Phase = "succeeded"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", batchLabel(b)+" 全部通过健康观察")
			return true
		}
		return false

	case "paused":
		// A batch waits here while a human reviews the evidence. Every choice
		// is persisted on the release, so a page reopen shows the same frozen
		// state and pressing a button twice can't change a made decision.
		switch {
		case rel.CancelRequested:
			rel.ResumeRequested = false
			e.startBatchRollback(tx, rel, b, now, "审查中取消发布：当前批次回退，之后到达的健康结果仅留档")
			return true
		case rel.ResumeRequested || rel.ReviewAction == "rollback":
			rel.ResumeRequested = false
			e.startBatchRollback(tx, rel, b, now, "审查完成，继续批次回退")
			return true
		case rel.FinishRequested || rel.ReviewAction == "finish":
			b.Phase = "failed"
			b.FinishedAt = nowStr(now)
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "error", "审查后结束本次演练：保留当前版本分布，不执行回退")
			return true
		case rel.ReviewAction == "continue" || rel.ReviewAction == "shrink":
			rel.PauseRequested = false
			rel.Status = "in_progress"
			anyUpdated := false
			for i := range b.Members {
				if b.Members[i].UpdateState == "succeeded" {
					anyUpdated = true
				}
			}
			if anyUpdated {
				b.Phase = "observing"
			} else {
				b.Phase = "updating"
			}
			tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", batchLabel(b)+" 审查暂停结束，按决定继续推进（不重复已完成更新）")
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
func (e *Engine) pauseBatch(tx *store.Tx, rel *store.Release, b *store.Batch, now time.Time, reason string) {
	b.Phase = "paused"
	rel.Status = "paused"
	rel.Message = "已暂停：可审查失败实例，再选择继续回退或结束本次演练"
	tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", batchLabel(b)+" 因"+reason+"暂停，等待人工审查")
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
