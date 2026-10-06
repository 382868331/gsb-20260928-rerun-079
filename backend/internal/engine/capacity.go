package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"releasectl/internal/store"
)

// zoneByID finds a zone by ID.
func zoneByID(tx *store.Tx, id string) *store.Zone {
	for i := range tx.Zones() {
		if tx.Zones()[i].ID == id {
			return &tx.Zones()[i]
		}
	}
	return nil
}

func zoneName(tx *store.Tx, id string) string {
	if z := zoneByID(tx, id); z != nil {
		return z.Name
	}
	return id
}

// occupiedInZone returns the set of instance IDs currently covered by held
// capacity grants in the zone, plus the holding release per instance.
func occupiedInZone(tx *store.Tx, zoneID string) map[string]string {
	out := map[string]string{}
	for _, g := range tx.Grants() {
		if g.ZoneID != zoneID || g.State != "held" {
			continue
		}
		for _, id := range g.InstanceIDs {
			out[id] = g.ReleaseID
		}
	}
	return out
}

// capacityCheck evaluates whether the given members (instance IDs) may be
// occupied in one zone right now. It returns a human-readable reason when
// the check fails, naming the zone, the numbers and the releases holding
// the conflicting commitments.
func capacityCheck(tx *store.Tx, zone *store.Zone, memberIDs []string) (string, bool) {
	if zone == nil || len(memberIDs) == 0 {
		return "", true
	}
	occupied := occupiedInZone(tx, zone.ID)
	byService := map[string][]string{}
	for _, inst := range tx.Instances() {
		if inst.ZoneID == zone.ID {
			byService[inst.ServiceID] = append(byService[inst.ServiceID], inst.ID)
		}
	}
	newByService := map[string]int{}
	for _, id := range memberIDs {
		if inst := findInstance(tx, id); inst != nil {
			newByService[inst.ServiceID]++
		}
	}
	services := make([]string, 0, len(newByService))
	for svc := range newByService {
		services = append(services, svc)
	}
	sort.Strings(services)
	holders := map[string]bool{}
	for _, relID := range occupied {
		holders[relID] = true
	}
	holderList := make([]string, 0, len(holders))
	for id := range holders {
		holderList = append(holderList, id)
	}
	sort.Strings(holderList)
	holderText := ""
	if len(holderList) > 0 {
		holderText = "；正被发布 " + strings.Join(holderList, "、") + " 占用"
	}
	for _, svc := range services {
		min, ok := zone.MinAvailable[svc]
		if !ok || min <= 0 {
			continue
		}
		held := 0
		for _, id := range byService[svc] {
			if _, taken := occupied[id]; taken {
				held++
			}
		}
		avail := len(byService[svc]) - held - newByService[svc]
		if avail < min {
			return fmt.Sprintf("故障区域 %s：%s 可用数将降至 %d 台（最低要求 %d 台，本批占用 %d 台%s），等待容量释放",
				zone.Name, serviceName(tx, svc), avail, min, newByService[svc], holderText), false
		}
	}
	if zone.SharedCapacity > 0 {
		total := len(occupied) + len(memberIDs)
		if total > zone.SharedCapacity {
			return fmt.Sprintf("故障区域 %s：跨服务共享容量将占 %d 台（上限 %d 台%s），等待容量释放",
				zone.Name, total, zone.SharedCapacity, holderText), false
		}
	}
	return "", true
}

// capacityBasis describes, per zone touched by the batch, which instances
// will be occupied and the numbers the acquire decision is based on.
func capacityBasis(tx *store.Tx, b *store.Batch) []string {
	byZone := map[string][]string{}
	var order []string
	for _, m := range b.Members {
		inst := findInstance(tx, m.InstanceID)
		zid := ""
		if inst != nil {
			zid = inst.ZoneID
		}
		if _, seen := byZone[zid]; !seen {
			order = append(order, zid)
		}
		byZone[zid] = append(byZone[zid], m.InstanceID)
	}
	sort.Strings(order)
	var out []string
	for _, zid := range order {
		ids := byZone[zid]
		sort.Strings(ids)
		if zid == "" {
			out = append(out, fmt.Sprintf("本批占用实例 %s（未分配故障区域，不参与容量核算）", strings.Join(ids, "、")))
			continue
		}
		z := zoneByID(tx, zid)
		occupied := occupiedInZone(tx, zid)
		shared := "无共享上限"
		if z != nil && z.SharedCapacity > 0 {
			shared = fmt.Sprintf("共享容量 %d/%d 台已占用", len(occupied), z.SharedCapacity)
		}
		out = append(out, fmt.Sprintf("故障区域 %s：本批将占用实例 %s（当前已被其他发布占用 %d 台，%s）",
			zoneName(tx, zid), strings.Join(ids, "、"), len(occupied), shared))
	}
	return out
}

// tryAcquireCapacity attempts to take capacity grants for a batch that is
// about to expand. On success the grants are persisted and returned; on
// failure nothing is written and the reason is returned for persistence on
// the batch (so a reopen shows the same waiting state).
func (e *Engine) tryAcquireCapacity(tx *store.Tx, rel *store.Release, b *store.Batch, now time.Time) (string, bool) {
	byZone := map[string][]string{}
	var order []string
	for _, m := range b.Members {
		inst := findInstance(tx, m.InstanceID)
		if inst == nil || inst.ZoneID == "" {
			continue
		}
		if _, seen := byZone[inst.ZoneID]; !seen {
			order = append(order, inst.ZoneID)
		}
		byZone[inst.ZoneID] = append(byZone[inst.ZoneID], m.InstanceID)
	}
	sort.Strings(order)
	for _, zid := range order {
		if reason, ok := capacityCheck(tx, zoneByID(tx, zid), byZone[zid]); !ok {
			return reason, false
		}
	}
	grants := tx.Grants()
	for _, zid := range order {
		ids := append([]string(nil), byZone[zid]...)
		sort.Strings(ids)
		g := store.CapacityGrant{
			ID: newID(tx, "grant"), ReleaseID: rel.ID, BatchID: b.ID, ZoneID: zid,
			InstanceIDs: ids, State: "held", AcquiredAt: nowStr(now),
		}
		grants = append(grants, g)
		b.GrantIDs = append(b.GrantIDs, g.ID)
		tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info",
			fmt.Sprintf("取得故障区域 %s 的容量承诺：占用实例 %s", zoneName(tx, zid), strings.Join(ids, "、")))
	}
	tx.SetGrants(grants)
	b.CapacityWait = ""
	b.CapacityWaitSince = ""
	return "", true
}

// releaseSettledGrants sweeps every held grant and drops instances whose
// real, persisted state is settled: verified healthy on the new version, or
// verified back on the original version (rolled back / never moved). A grant
// is released only when its last instance settles, so availability always
// reflects real instance state rather than a renamed plan.
func (e *Engine) releaseSettledGrants(tx *store.Tx, now time.Time) bool {
	changed := false
	grants := tx.Grants()
	for gi := range grants {
		g := &grants[gi]
		if g.State != "held" {
			continue
		}
		b := findBatch(tx, g.BatchID)
		kept := g.InstanceIDs[:0]
		for _, instID := range g.InstanceIDs {
			settled, why := memberSettled(tx, b, instID)
			if settled {
				changed = true
				tx.AddEvent(nowStr(now), g.ReleaseID, g.BatchID, instID, "info",
					fmt.Sprintf("实例在故障区域 %s 的容量占用释放：%s", zoneName(tx, g.ZoneID), why))
				continue
			}
			kept = append(kept, instID)
		}
		g.InstanceIDs = kept
		if len(g.InstanceIDs) == 0 {
			g.State = "released"
			g.ReleasedAt = nowStr(now)
			g.ReleaseWhy = "全部实例归宿已确认（健康通过或已回退原版本）"
			changed = true
			tx.AddEvent(nowStr(now), g.ReleaseID, g.BatchID, "", "info",
				fmt.Sprintf("故障区域 %s 的容量承诺已按实例真实状态全部释放", zoneName(tx, g.ZoneID)))
		}
	}
	if changed {
		tx.SetGrants(grants)
	}
	return changed
}

// memberSettled reports whether an instance's real state is final enough to
// free its capacity slot, with the evidence as the reason.
func memberSettled(tx *store.Tx, b *store.Batch, instanceID string) (bool, string) {
	inst := findInstance(tx, instanceID)
	if inst == nil {
		return true, "实例记录已不存在"
	}
	if b == nil {
		return true, "所属批次已不存在"
	}
	var m *store.MemberState
	for i := range b.Members {
		if b.Members[i].InstanceID == instanceID {
			m = &b.Members[i]
			break
		}
	}
	if m == nil {
		return true, "实例不再属于该批次"
	}
	switch {
	case m.UpdateState == "rolled_back":
		return true, "实例已确认回到原版本 " + inst.CurrentVersion
	case m.HealthState == "healthy" && m.UpdateState == "succeeded" && inst.CurrentVersion == m.ToVersion:
		return true, "实例已在新版本 " + inst.CurrentVersion + " 健康通过"
	case m.UpdateState == "failed" && inst.CurrentVersion == m.FromVersion:
		return true, "实例更新失败且确认停留在原版本 " + inst.CurrentVersion
	}
	if isBatchTerminal(b.Phase) && m.UpdateState != "in_progress" && m.UpdateState != "rollback_in_progress" {
		return true, "批次已终止，实例真实版本确认为 " + inst.CurrentVersion
	}
	return false, ""
}

// spreadAcrossZones orders instances round-robin over their zones so each
// batch spreads its footprint across failure zones instead of draining one
// zone first.
func spreadAcrossZones(insts []store.Instance) []store.Instance {
	byZone := map[string][]store.Instance{}
	var zones []string
	for _, inst := range insts {
		if _, seen := byZone[inst.ZoneID]; !seen {
			zones = append(zones, inst.ZoneID)
		}
		byZone[inst.ZoneID] = append(byZone[inst.ZoneID], inst)
	}
	sort.Strings(zones)
	var out []store.Instance
	for i := 0; len(out) < len(insts); i++ {
		for _, z := range zones {
			list := byZone[z]
			if i < len(list) {
				out = append(out, list[i])
			}
		}
	}
	return out
}
