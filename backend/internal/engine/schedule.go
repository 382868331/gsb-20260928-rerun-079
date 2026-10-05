package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"releasectl/internal/store"
)

// maxPlanRecords bounds the persisted re-plan history.
const maxPlanRecords = 30

// agingUnit is the waiting time that compensates one priority level: a
// release waiting this long gains +1 effective priority, so a low-priority
// release always outranks higher-priority ones eventually and can never
// starve.
func (e *Engine) agingUnit() time.Duration {
	if e.c.AgingUnit > 0 {
		return e.c.AgingUnit
	}
	return 60 * time.Second
}

// effectivePriority ranks releases competing for the same zone capacity.
// Base priority dominates; accumulated waiting time is added on top so
// aging eventually lets any waiting release win admission.
func (e *Engine) effectivePriority(tx *store.Tx, rel *store.Release, now time.Time) int64 {
	eff := int64(rel.Priority) * e.agingUnit().Milliseconds()
	b := headPendingBatch(tx, rel.ID)
	if b == nil || b.CapacityWaitSince == "" {
		return eff
	}
	since, err := time.Parse(time.RFC3339Nano, b.CapacityWaitSince)
	if err != nil {
		return eff
	}
	if waited := now.Sub(since).Milliseconds(); waited > 0 {
		eff += waited
	}
	return eff
}

// batchZones returns the sorted zone IDs a batch's members live in.
func batchZones(tx *store.Tx, b *store.Batch) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range b.Members {
		if inst := findInstance(tx, m.InstanceID); inst != nil && inst.ZoneID != "" && !seen[inst.ZoneID] {
			seen[inst.ZoneID] = true
			out = append(out, inst.ZoneID)
		}
	}
	sort.Strings(out)
	return out
}

// headPendingBatch returns the batch a release would expand next: the first
// non-terminal batch, if it is still pending (never started).
func headPendingBatch(tx *store.Tx, releaseID string) *store.Batch {
	batches := releaseBatches(tx, releaseID)
	sortBatches(batches)
	for _, b := range batches {
		if isBatchTerminal(b.Phase) {
			if b.Phase != "succeeded" {
				return nil
			}
			continue
		}
		if b.Phase != "pending" {
			return nil
		}
		return b
	}
	return nil
}

// shouldYield decides whether a pending batch must hold back because a
// release with higher effective priority is queued for the same zone
// capacity. Only not-yet-executed batches are reordered this way; batches
// already updating or rolling back are never preempted.
//
// A full aging unit of hysteresis is required before yielding: an
// in-progress release's next batch is never jumped by a merely equal
// competitor, but a waiting release that has aged a whole level beyond it
// (aging) always wins, so starvation is impossible.
func (e *Engine) shouldYield(tx *store.Tx, rel *store.Release, b *store.Batch, now time.Time) (string, bool) {
	myZones := map[string]bool{}
	for _, z := range batchZones(tx, b) {
		myZones[z] = true
	}
	if len(myZones) == 0 {
		return "", false
	}
	myEff := e.effectivePriority(tx, rel, now)
	threshold := myEff + e.agingUnit().Milliseconds()
	var best *store.Release
	var bestZones []string
	for i := range tx.Releases() {
		o := &tx.Releases()[i]
		if o.ID == rel.ID || isReleaseTerminal(o.Status) {
			continue
		}
		// A release frozen for review or being cancelled is not competing
		// for capacity right now; yielding to it would deadlock the queue.
		if o.Status == "paused" || o.CancelRequested {
			continue
		}
		if e.effectivePriority(tx, o, now) < threshold {
			continue
		}
		ob := headPendingBatch(tx, o.ID)
		if ob == nil {
			continue
		}
		// Yielding only makes sense under real contention: the competing
		// release must itself be blocked on capacity right now. While its
		// batches can still acquire freely there is nothing to reorder.
		if ob.CapacityWait == "" {
			continue
		}
		var overlap []string
		for _, z := range batchZones(tx, ob) {
			if myZones[z] {
				overlap = append(overlap, zoneName(tx, z))
			}
		}
		if len(overlap) == 0 {
			continue
		}
		if best == nil || e.effectivePriority(tx, o, now) > e.effectivePriority(tx, best, now) {
			best = o
			bestZones = overlap
		}
	}
	if best == nil {
		return "", false
	}
	waited := ""
	if hb := headPendingBatch(tx, best.ID); hb != nil && hb.CapacityWaitSince != "" {
		if since, err := time.Parse(time.RFC3339Nano, hb.CapacityWaitSince); err == nil {
			waited = fmt.Sprintf("，已等待 %d 秒", int64(now.Sub(since).Seconds()))
		}
	}
	return fmt.Sprintf("让位于更高优先级的发布 %s（服务 %s，优先级 %d%s）：其下一批次与本批竞争故障区域 %s 的共享容量，本批暂不扩批",
		best.ID, serviceName(tx, best.ServiceID), best.Priority, waited, strings.Join(bestZones, "、")), true
}

// simInterval is one occupied capacity slot window used by the planner.
type simInterval struct {
	releaseID string
	instances []string
	freeAt    time.Time
}

// Replan recomputes the coordinated cross-release batch order and persists
// it as a new plan record together with the concrete capacity conflicts.
// It only ever reorders not-yet-executed batches: in-flight updates and
// rollbacks keep their acquired commitments untouched.
func (e *Engine) Replan(tx *store.Tx, now time.Time, trigger string) {
	record := store.PlanRecord{
		ID: newID(tx, "plan"), At: nowStr(now), Trigger: trigger,
		Entries: []store.PlanEntry{}, Conflicts: []store.PlanConflict{},
	}
	batchDur := e.c.UpdateDelay + e.c.HealthDelay

	// Occupancy starts from the commitments actually held right now. They
	// are honoured as-is (never replanned away) and estimated to free once
	// the holding batch settles by real instance state.
	occupied := map[string][]simInterval{} // zoneID -> intervals
	addInterval := func(zoneID, releaseID string, ids []string, freeAt time.Time) {
		occupied[zoneID] = append(occupied[zoneID], simInterval{releaseID: releaseID, instances: ids, freeAt: freeAt})
	}
	for _, g := range tx.Grants() {
		if g.State != "held" || len(g.InstanceIDs) == 0 {
			continue
		}
		addInterval(g.ZoneID, g.ReleaseID, append([]string(nil), g.InstanceIDs...), now.Add(batchDur))
	}

	// Collect the executable queue: pending batches of every release that
	// is neither finished nor frozen for review/cancellation.
	type queueItem struct {
		rel     *store.Release
		batches []*store.Batch
		next    int
		startAt time.Time
	}
	var queue []*queueItem
	for i := range tx.Releases() {
		rel := &tx.Releases()[i]
		if isReleaseTerminal(rel.Status) || rel.Status == "paused" || rel.CancelRequested {
			continue
		}
		qi := &queueItem{rel: rel, startAt: now}
		batches := releaseBatches(tx, rel.ID)
		sortBatches(batches)
		for _, b := range batches {
			if b.Phase == "pending" {
				qi.batches = append(qi.batches, b)
			}
		}
		if len(qi.batches) > 0 {
			queue = append(queue, qi)
		}
	}

	occupiedAt := func(zoneID string, t time.Time) (map[string]string, []string) {
		inst := map[string]string{}
		var holders []string
		seen := map[string]bool{}
		for _, iv := range occupied[zoneID] {
			if !iv.freeAt.After(t) {
				continue
			}
			for _, id := range iv.instances {
				inst[id] = iv.releaseID
			}
			if !seen[iv.releaseID] {
				seen[iv.releaseID] = true
				holders = append(holders, iv.releaseID)
			}
		}
		sort.Strings(holders)
		return inst, holders
	}
	candidateTimes := func(earliest time.Time) []time.Time {
		set := map[time.Time]bool{earliest: true}
		for _, ivs := range occupied {
			for _, iv := range ivs {
				if iv.freeAt.After(earliest) {
					set[iv.freeAt] = true
				}
			}
		}
		out := make([]time.Time, 0, len(set))
		for t := range set {
			out = append(out, t)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
		return out
	}

	order := 0
	estFinish := map[string]time.Time{}
	for {
		var pick *queueItem
		for _, qi := range queue {
			if qi.next >= len(qi.batches) {
				continue
			}
			if pick == nil || e.effectivePriority(tx, qi.rel, now) > e.effectivePriority(tx, pick.rel, now) {
				pick = qi
			}
		}
		if pick == nil {
			break
		}
		b := pick.batches[pick.next]
		zones := batchZones(tx, b)
		byZone := map[string][]string{}
		for _, m := range b.Members {
			if inst := findInstance(tx, m.InstanceID); inst != nil && inst.ZoneID != "" {
				byZone[inst.ZoneID] = append(byZone[inst.ZoneID], m.InstanceID)
			}
		}
		placed := false
		var lastReason string
		for _, t := range candidateTimes(pick.startAt) {
			fits := true
			for _, zid := range zones {
				occ, holders := occupiedAt(zid, t)
				if reason, ok := capacityCheckOccupied(tx, zoneByID(tx, zid), byZone[zid], occ, holders); !ok {
					lastReason = reason
					fits = false
					break
				}
			}
			if !fits {
				continue
			}
			end := t.Add(batchDur)
			for _, zid := range zones {
				addInterval(zid, pick.rel.ID, byZone[zid], end)
			}
			order++
			note := "可立即取得容量执行"
			if t.After(now) {
				note = "等待容量归还后执行"
				if len(zones) > 0 {
					_, holders := occupiedAt(zones[0], now)
					if len(holders) > 0 {
						note = fmt.Sprintf("等待发布 %s 归还容量后执行", strings.Join(holders, "、"))
					}
				}
			}
			zoneNames := make([]string, 0, len(zones))
			for _, zid := range zones {
				zoneNames = append(zoneNames, zoneName(tx, zid))
			}
			record.Entries = append(record.Entries, store.PlanEntry{
				Order: order, ReleaseID: pick.rel.ID, ServiceName: serviceName(tx, pick.rel.ServiceID),
				BatchID: b.ID, BatchIndex: b.Index, Priority: pick.rel.Priority,
				Zones: zoneNames, EstimatedStart: nowStr(t), EstimatedEnd: nowStr(end), Note: note,
			})
			pick.startAt = end
			if cur, ok := estFinish[pick.rel.ID]; !ok || end.After(cur) {
				estFinish[pick.rel.ID] = end
			}
			placed = true
			break
		}
		if !placed {
			// Distinguish "impossible even on empty capacity" from "busy
			// right now": the former is a configuration conflict, the
			// latter a transient one that names the current holders.
			impossible := false
			reason := lastReason
			for _, zid := range zones {
				if r, ok := capacityCheckOccupied(tx, zoneByID(tx, zid), byZone[zid], map[string]string{}, nil); !ok {
					impossible = true
					reason = r
					break
				}
			}
			if !impossible && reason == "" {
				reason = "容量持续被其他发布占用"
			}
			rest := len(pick.batches) - pick.next
			if impossible {
				reason = "即使全部容量空闲也无法满足约束：" + reason
			}
			if rest > 1 {
				reason += fmt.Sprintf("（该发布后续还有 %d 个批次同样无法安排）", rest-1)
			}
			record.Conflicts = append(record.Conflicts, store.PlanConflict{
				ReleaseID: pick.rel.ID, ServiceName: serviceName(tx, pick.rel.ServiceID),
				BatchID: b.ID, BatchIndex: b.Index, Reason: reason,
			})
			pick.next = len(pick.batches)
			continue
		}
		pick.next++
	}

	// Deadline feasibility: a release whose planned finish passes its
	// latest acceptable time gets a concrete conflict naming the bottleneck.
	for _, qi := range queue {
		if qi.rel.Deadline == "" {
			continue
		}
		deadline, err := time.Parse(time.RFC3339Nano, qi.rel.Deadline)
		if err != nil {
			continue
		}
		finish, ok := estFinish[qi.rel.ID]
		if !ok {
			continue // unschedulable batches already produced a conflict
		}
		if !finish.After(deadline) {
			continue
		}
		bottleneck := ""
		latest := time.Time{}
		latestRel := ""
		latestZone := ""
		for _, b := range qi.batches {
			for _, zid := range batchZones(tx, b) {
				for _, iv := range occupied[zid] {
					if iv.releaseID != qi.rel.ID && iv.freeAt.After(latest) && iv.freeAt.After(now) {
						latest = iv.freeAt
						latestRel = iv.releaseID
						latestZone = zid
					}
				}
			}
		}
		if latestRel != "" {
			bottleneck = fmt.Sprintf("；瓶颈：故障区域 %s 的容量被发布 %s 占用至 %s",
				zoneName(tx, latestZone), latestRel, latest.UTC().Format("15:04:05"))
		}
		record.Conflicts = append(record.Conflicts, store.PlanConflict{
			ReleaseID: qi.rel.ID, ServiceName: serviceName(tx, qi.rel.ServiceID),
			BatchID: qi.batches[len(qi.batches)-1].ID, BatchIndex: qi.batches[len(qi.batches)-1].Index,
			DeadlineMiss: true,
			Reason: fmt.Sprintf("预计 %s 完成，超过最迟完成时间 %s%s",
				finish.UTC().Format("15:04:05"), deadline.UTC().Format("15:04:05"), bottleneck),
		})
	}

	plans := append(tx.Plans(), record)
	if len(plans) > maxPlanRecords {
		plans = plans[len(plans)-maxPlanRecords:]
	}
	tx.SetPlans(plans)
	level := "info"
	if len(record.Conflicts) > 0 {
		level = "warn"
	}
	tx.AddEvent(nowStr(now), "", "", "", level,
		fmt.Sprintf("协调重排（%s）：%d 个待执行批次纳入顺序，%d 个容量冲突", trigger, len(record.Entries), len(record.Conflicts)))
}
