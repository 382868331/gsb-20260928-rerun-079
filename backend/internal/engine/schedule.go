package engine

import (
	"fmt"
	"sort"
	"time"

	"releasectl/internal/store"
)

// agingBoostPerMinute is the effective-priority gain a waiting release earns
// per minute of capacity wait. One priority level equals 100 points, so a
// low-priority release that keeps waiting eventually overtakes a fresher
// higher-priority one (starvation prevention), while a just-arrived
// high-priority release still reorders not-yet-started batches first.
const agingBoostPerMinute = 5

// maxAgingBoost caps the aging gain at two priority levels.
const maxAgingBoost = 200

// waitSince returns when the release started waiting for capacity: the
// oldest persisted CapacityWaitSince of its pending batches, or — for a
// release that has not started any batch yet — its creation time.
func waitSince(tx *store.Tx, rel *store.Release, now time.Time) time.Time {
	oldest := time.Time{}
	for _, b := range releaseBatches(tx, rel.ID) {
		if b.Phase != "pending" || b.CapacityWaitSince == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, b.CapacityWaitSince); err == nil {
			if oldest.IsZero() || t.Before(oldest) {
				oldest = t
			}
		}
	}
	if oldest.IsZero() {
		started := false
		for _, b := range releaseBatches(tx, rel.ID) {
			if b.Phase != "pending" {
				started = true
			}
		}
		if !started {
			if t, err := time.Parse(time.RFC3339Nano, rel.CreatedAt); err == nil {
				oldest = t
			}
		}
	}
	if oldest.IsZero() {
		oldest = now
	}
	return oldest
}

// effectivePriority is the scheduling score: operator priority dominates,
// aging breaks long waits out of starvation.
func effectivePriority(tx *store.Tx, rel *store.Release, now time.Time) int {
	boost := int(now.Sub(waitSince(tx, rel, now)).Minutes()) * agingBoostPerMinute
	if boost > maxAgingBoost {
		boost = maxAgingBoost
	}
	if boost < 0 {
		boost = 0
	}
	return rel.Priority*100 + boost
}

// coordinatedReleases orders in-flight releases for capacity acquisition:
// higher effective priority first, then earlier deadline, then creation
// order. Only not-yet-started batches are affected by this order; batches
// already updating or rolling back are never interrupted.
func coordinatedReleases(tx *store.Tx, now time.Time) []*store.Release {
	var out []*store.Release
	for i := range tx.Releases() {
		rel := &tx.Releases()[i]
		if isReleaseTerminal(rel.Status) {
			continue
		}
		out = append(out, rel)
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := effectivePriority(tx, out[i], now), effectivePriority(tx, out[j], now)
		if pi != pj {
			return pi > pj
		}
		di, dj := out[i].Deadline, out[j].Deadline
		if di != dj {
			if di == "" {
				return false
			}
			if dj == "" {
				return true
			}
			return di < dj
		}
		if ci, cj := out[i].CreatedAt, out[j].CreatedAt; ci != cj {
			// RFC3339 strings with fractional seconds do not order
			// lexicographically ("….00.15Z" < "….00Z"); compare as time.
			ti, ei := time.Parse(time.RFC3339Nano, ci)
			tj, ej := time.Parse(time.RFC3339Nano, cj)
			if ei == nil && ej == nil && !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return ci < cj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// batchDuration estimates how long one batch occupies its capacity slots.
func (e *Engine) batchDuration() time.Duration {
	return e.c.UpdateDelay + e.c.HealthDelay
}

// BuildSchedule computes the coordinated multi-release execution plan from
// the persisted state. It is read-only: nothing is acquired, released or
// reordered here. The queue shows the executable batch order; the estimates
// name the concrete capacity conflict when a deadline cannot be met.
func (e *Engine) BuildSchedule(tx *store.Tx, now time.Time) store.ScheduleView {
	view := store.ScheduleView{
		GeneratedAt: nowStr(now),
		Queue:       []store.ScheduleItem{},
		Estimates:   []store.ReleaseEstimate{},
	}
	rels := coordinatedReleases(tx, now)
	// Simulated occupancy: zoneID -> instanceID -> free time. Starts from
	// the real held commitments; simulated batches occupy and free slots
	// as the plan walks forward in virtual time.
	type hold struct {
		freeAt time.Time
	}
	simHeld := map[string]map[string]hold{}
	for _, g := range tx.Grants() {
		if g.State != "held" {
			continue
		}
		if simHeld[g.ZoneID] == nil {
			simHeld[g.ZoneID] = map[string]hold{}
		}
		for _, id := range g.InstanceIDs {
			// Real grants free at an unknown time; assume one batch
			// duration from now for the estimate.
			simHeld[g.ZoneID][id] = hold{freeAt: now.Add(e.batchDuration())}
		}
	}
	dur := e.batchDuration()
	order := 0
	for _, rel := range rels {
		eff := effectivePriority(tx, rel, now)
		est := store.ReleaseEstimate{
			ReleaseID:   rel.ID,
			ServiceName: serviceName(tx, rel.ServiceID),
			Priority:    rel.Priority,
			Deadline:    rel.Deadline,
			OnTime:      true,
		}
		cursor := now
		batches := releaseBatches(tx, rel.ID)
		sortBatches(batches)
		conflictSeen := map[string]bool{}
		for _, b := range batches {
			if isBatchTerminal(b.Phase) {
				continue
			}
			item := store.ScheduleItem{
				Order:             order,
				ReleaseID:         rel.ID,
				ServiceID:         rel.ServiceID,
				ServiceName:       serviceName(tx, rel.ServiceID),
				BatchID:           b.ID,
				BatchIndex:        b.Index,
				Priority:          rel.Priority,
				EffectivePriority: eff,
			}
			order++
			if b.Phase != "pending" {
				item.State = "running"
				item.Reason = "批次正在执行：更新/回退中的实例不可被其他发布抢占"
				view.Queue = append(view.Queue, item)
				continue
			}
			// Simulate this batch starting once the release's earlier
			// batches are done; conflicts push the estimate out and are
			// reported with the zone, the numbers and the holder.
			start := cursor
			byZone := map[string][]string{}
			var zones []string
			for _, m := range b.Members {
				inst := findInstance(tx, m.InstanceID)
				if inst == nil || inst.ZoneID == "" {
					continue
				}
				if _, seen := byZone[inst.ZoneID]; !seen {
					zones = append(zones, inst.ZoneID)
				}
				byZone[inst.ZoneID] = append(byZone[inst.ZoneID], m.InstanceID)
			}
			sort.Strings(zones)
			conflict := ""
			for _, zid := range zones {
				var heldIDs []string
				for id, h := range simHeld[zid] {
					if h.freeAt.After(start) {
						heldIDs = append(heldIDs, id)
					}
				}
				if reason, ok := capacityCheckWith(tx, zoneByID(tx, zid), byZone[zid], heldIDs); !ok {
					conflict = reason
					break
				}
			}
			if conflict != "" {
				item.State = "conflict"
				item.Reason = conflict
				if !conflictSeen[conflict] {
					conflictSeen[conflict] = true
					est.Conflicts = append(est.Conflicts, fmt.Sprintf("第 %d 批：%s", b.Index+1, conflict))
				}
				// Assume the conflict costs one extra batch duration of
				// waiting before capacity frees by real instance state.
				start = start.Add(dur)
			} else if b.CapacityWait != "" {
				item.State = "waiting"
				item.Reason = b.CapacityWait
			} else if cursor.Equal(now) {
				item.State = "ready"
			} else {
				item.State = "waiting"
				item.Reason = "排在本发布更早批次之后"
			}
			finish := start.Add(dur)
			for zid, ids := range byZone {
				if simHeld[zid] == nil {
					simHeld[zid] = map[string]hold{}
				}
				for _, id := range ids {
					simHeld[zid][id] = hold{freeAt: finish}
				}
			}
			cursor = finish
			view.Queue = append(view.Queue, item)
		}
		est.EstimatedFinish = nowStr(cursor)
		if rel.Deadline != "" {
			if dl, err := time.Parse(time.RFC3339Nano, rel.Deadline); err == nil {
				est.OnTime = !cursor.After(dl)
			}
		}
		view.Estimates = append(view.Estimates, est)
	}
	return view
}

// emitReorderAssessment re-evaluates the next pending batch of every
// in-flight release after a runtime change (zone capacity shrink/growth or
// a cancelled release) and persists the reorder reason per batch. Only
// not-yet-started batches are assessed: held commitments and in-flight
// instances are untouched, and every other service's minimum-available
// constraint is re-checked exactly as a fresh acquisition would be.
func (e *Engine) emitReorderAssessment(tx *store.Tx, now time.Time, cause string) {
	for _, rel := range coordinatedReleases(tx, now) {
		if rel.CancelRequested {
			continue
		}
		batches := releaseBatches(tx, rel.ID)
		sortBatches(batches)
		for _, b := range batches {
			if isBatchTerminal(b.Phase) {
				if b.Phase != "succeeded" {
					break
				}
				continue
			}
			if b.Phase != "pending" {
				// In-flight batches are never reordered; the assessment
				// targets the next not-yet-started batch behind them.
				continue
			}
			byZone := map[string][]string{}
			var zones []string
			for _, m := range b.Members {
				inst := findInstance(tx, m.InstanceID)
				if inst == nil || inst.ZoneID == "" {
					continue
				}
				if _, seen := byZone[inst.ZoneID]; !seen {
					zones = append(zones, inst.ZoneID)
				}
				byZone[inst.ZoneID] = append(byZone[inst.ZoneID], m.InstanceID)
			}
			sort.Strings(zones)
			reason := ""
			ok := true
			for _, zid := range zones {
				if r, ok2 := capacityCheck(tx, zoneByID(tx, zid), byZone[zid]); !ok2 {
					reason, ok = r, false
					break
				}
			}
			if ok {
				if b.CapacityWait != "" {
					tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info",
						"重排评估（"+cause+"）："+batchLabel(b)+" 现在可获得容量，将在下一调度周期扩批；此前等待原因已失效")
				} else {
					tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info",
						"重排评估（"+cause+"）："+batchLabel(b)+" 容量可满足，执行顺序不变")
				}
			} else {
				tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn",
					"重排评估（"+cause+"）："+batchLabel(b)+" 继续等待——"+reason)
			}
			break // only the next pending batch per release is a candidate
		}
	}
}
