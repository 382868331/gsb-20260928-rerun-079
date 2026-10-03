package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"releasectl/internal/store"
)

// zoneDemand groups the batch member instance IDs by their zone.
func zoneDemand(tx *store.Tx, b *store.Batch) map[string][]string {
	demand := map[string][]string{}
	for _, m := range b.Members {
		zoneID := ""
		if inst := findInstance(tx, m.InstanceID); inst != nil {
			zoneID = inst.ZoneID
		}
		demand[zoneID] = append(demand[zoneID], m.InstanceID)
	}
	return demand
}

func findPolicy(tx *store.Tx, zoneID string) *store.CapacityPolicy {
	for i := range tx.Policies() {
		if tx.Policies()[i].ZoneID == zoneID {
			return &tx.Policies()[i]
		}
	}
	return nil
}

func zoneName(tx *store.Tx, id string) string {
	for _, z := range tx.Zones() {
		if z.ID == id {
			return z.Name
		}
	}
	if id == "" {
		return "未分区"
	}
	return id
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// capacityCheck evaluates whether the batch may occupy zone capacity: every
// zone it touches must keep the configured per-service minimum available and
// stay within the shared cross-service pool. The check counts capacity
// already committed to other releases (held reservations), so two plans that
// are each safe alone cannot both pass when together they breach a limit.
func capacityCheck(tx *store.Tx, rel *store.Release, b *store.Batch) (bool, string) {
	demand := zoneDemand(tx, b)
	var reasons []string
	for _, zoneID := range sortedKeys(demand) {
		need := len(demand[zoneID])
		minAvail, sharedMax := 0, 0
		if pol := findPolicy(tx, zoneID); pol != nil {
			minAvail = pol.MinAvailable[rel.ServiceID]
			sharedMax = pol.SharedMax
		}
		heldSvc, heldAll := 0, 0
		heldBy := map[string]int{}
		for _, r := range tx.Reservations() {
			if r.State != "held" || r.ZoneID != zoneID {
				continue
			}
			if r.BatchID == b.ID {
				continue // a batch never competes with its own commitment
			}
			heldAll += len(r.Members)
			heldBy[r.ReleaseID] += len(r.Members)
			if r.ServiceID == rel.ServiceID {
				heldSvc += len(r.Members)
			}
		}
		totalSvc := 0
		for _, inst := range tx.Instances() {
			if inst.ServiceID == rel.ServiceID && inst.ZoneID == zoneID {
				totalSvc++
			}
		}
		avail := totalSvc - heldSvc - need
		zone := zoneName(tx, zoneID)
		if avail < minAvail {
			reasons = append(reasons, fmt.Sprintf(
				"区域 %s：%s 最低可用 %d 台，其他发布已占用 %d 台，本批再占 %d 台后仅剩 %d 台",
				zone, serviceName(tx, rel.ServiceID), minAvail, heldSvc, need, avail))
		}
		if sharedMax > 0 && heldAll+need > sharedMax {
			var holders []string
			for relID, n := range heldBy {
				holders = append(holders, fmt.Sprintf("%s 占 %d 台", relID, n))
			}
			sort.Strings(holders)
			reasons = append(reasons, fmt.Sprintf(
				"区域 %s：跨服务共享容量 %d 台，已承诺 %d 台（%s），本批需 %d 台，超出下限",
				zone, sharedMax, heldAll, strings.Join(holders, "，"), need))
		}
	}
	return len(reasons) == 0, strings.Join(reasons, "；")
}

// grantCapacity persists one queryable reservation per zone the batch
// touches. The basis records the real state of every occupied instance at
// grant time.
func grantCapacity(tx *store.Tx, rel *store.Release, b *store.Batch, now time.Time) {
	demand := zoneDemand(tx, b)
	for _, zoneID := range sortedKeys(demand) {
		exists := false
		for _, r := range tx.Reservations() {
			if r.BatchID == b.ID && r.ZoneID == zoneID && r.State == "held" {
				exists = true
			}
		}
		if exists {
			continue
		}
		members := demand[zoneID]
		sort.Strings(members)
		res := store.CapacityReservation{
			ID: newID(tx, "res"), ReleaseID: rel.ID, BatchID: b.ID,
			ZoneID: zoneID, ServiceID: rel.ServiceID,
			Members: members, State: "held", CreatedAt: nowStr(now),
		}
		for _, id := range members {
			inst := findInstance(tx, id)
			if inst == nil {
				res.Basis = append(res.Basis, id+"：实例记录缺失")
				continue
			}
			res.Basis = append(res.Basis, fmt.Sprintf("%s 当前 %s，健康状态 %s，本批占用至批次生效",
				inst.ID, inst.CurrentVersion, healthText(inst.Health)))
		}
		tx.SetReservations(append(tx.Reservations(), res))
		tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "info", fmt.Sprintf(
			"获得区域容量承诺 %s（区域 %s，占用 %s）",
			res.ID, zoneName(tx, zoneID), strings.Join(members, "、")))
	}
}

// sweepReservations releases held commitments whose batch reached a terminal
// phase AND whose member instances are verified stable on disk (no update or
// rollback still in flight). Pauses keep the commitment held; renaming a plan
// never frees capacity — only the real member states do. Runs every tick so a
// crash between batch completion and release still resolves on restart.
func (e *Engine) sweepReservations(tx *store.Tx, now time.Time) {
	for i := range tx.Reservations() {
		res := &tx.Reservations()[i]
		if res.State != "held" {
			continue
		}
		b := findBatch(tx, res.BatchID)
		if b == nil || !isBatchTerminal(b.Phase) {
			continue
		}
		stable := true
		for _, m := range b.Members {
			if m.UpdateState == "in_progress" || m.UpdateState == "rollback_in_progress" {
				stable = false
			}
		}
		if !stable {
			continue
		}
		res.State = "released"
		res.ReleasedAt = nowStr(now)
		res.ReleaseReason = releaseReasonFor(b.Phase)
		for _, m := range b.Members {
			inst := findInstance(tx, m.InstanceID)
			if inst == nil {
				res.ReleaseBasis = append(res.ReleaseBasis, m.InstanceID+"：实例记录缺失")
				continue
			}
			res.ReleaseBasis = append(res.ReleaseBasis, fmt.Sprintf(
				"%s 真实状态：当前 %s，健康状态 %s，成员归宿 %s",
				inst.ID, inst.CurrentVersion, healthText(inst.Health), memberStateText(m.UpdateState)))
		}
		tx.AddEvent(nowStr(now), res.ReleaseID, res.BatchID, "", "info", fmt.Sprintf(
			"释放区域容量承诺 %s（区域 %s）：%s", res.ID, zoneName(tx, res.ZoneID), res.ReleaseReason))
	}
}

func releaseReasonFor(phase string) string {
	switch phase {
	case "succeeded":
		return "批次成功，实例均健康运行新版本"
	case "rolled_back":
		return "回退完成，实例均已返回原版本"
	case "cancelled":
		return "发布取消，实例归宿已确定为原版本"
	case "failed":
		return "演练结束，保留当前真实版本分布"
	}
	return "批次终止"
}

func memberStateText(s string) string {
	switch s {
	case "succeeded":
		return "已更新"
	case "rolled_back":
		return "已回退"
	case "failed":
		return "更新失败（未离开原版本）"
	case "pending":
		return "未更新"
	}
	return s
}
