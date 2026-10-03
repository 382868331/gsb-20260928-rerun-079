package engine

import (
	"fmt"
	"sort"
	"strings"

	"releasectl/internal/store"
)

// Capacity accounting lives entirely on persisted state. An instance is
// "occupied" while a prepared-but-not-yet-effective batch still holds it:
// the batch is updating, observing, paused for review, or rolling back, and
// the member has not reached its rolled-back destination. Flipping a
// release status never frees capacity; only the member's real recovery does.

// holder identifies the batch that currently occupies an instance.
type holder struct {
	releaseID string
	batchID   string
	index     int
}

// occupancyMap derives, from on-disk batch/member state, which instances
// are still actually occupied by an in-flight batch.
func occupancyMap(tx *store.Tx) map[string]holder {
	out := map[string]holder{}
	for i := range tx.Batches() {
		b := &tx.Batches()[i]
		switch b.Phase {
		case "updating", "observing", "paused", "rolling_back", "cancelling":
		default:
			continue
		}
		for _, m := range b.Members {
			// rolled_back members are recovered: their commitment is
			// released. rollback_in_progress members are still occupied —
			// they are not available again until the rollback op lands.
			if m.UpdateState == "rolled_back" {
				continue
			}
			out[m.InstanceID] = holder{releaseID: b.ReleaseID, batchID: b.ID, index: b.Index}
		}
	}
	return out
}

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
	if id == "" {
		return "未分区"
	}
	return id
}

func zoneIsFailed(tx *store.Tx, id string) bool {
	z := zoneByID(tx, id)
	return z != nil && z.Failed
}

// zoneStat is the availability ledger of one service in one zone.
type zoneStat struct {
	total       int
	occupied    int
	unavailable int // not occupied but not usable (unhealthy, unknown, failed zone)
	available   int
}

type zoneServiceKey struct {
	serviceID string
	zoneID    string
}

// zoneStats computes the live ledger for every service+zone pair that has
// instances. available = healthy, not occupied, zone not failed.
func zoneStats(tx *store.Tx, occ map[string]holder) map[zoneServiceKey]*zoneStat {
	out := map[zoneServiceKey]*zoneStat{}
	for _, inst := range tx.Instances() {
		k := zoneServiceKey{inst.ServiceID, inst.ZoneID}
		st := out[k]
		if st == nil {
			st = &zoneStat{}
			out[k] = st
		}
		st.total++
		if _, held := occ[inst.ID]; held {
			st.occupied++
			continue
		}
		if inst.Health != "healthy" || zoneIsFailed(tx, inst.ZoneID) {
			st.unavailable++
			continue
		}
		st.available++
	}
	return out
}

func statFor(stats map[zoneServiceKey]*zoneStat, serviceID, zoneID string) *zoneStat {
	if st := stats[zoneServiceKey{serviceID, zoneID}]; st != nil {
		return st
	}
	return &zoneStat{}
}

// holdersText explains which releases still hold commitments inside the
// given services+zone, so a blocked plan can name what it waits for.
func holdersText(tx *store.Tx, occ map[string]holder, serviceIDs []string, zoneID string) string {
	count := map[string]int{}
	for _, inst := range tx.Instances() {
		if inst.ZoneID != zoneID {
			continue
		}
		inScope := false
		for _, s := range serviceIDs {
			if inst.ServiceID == s {
				inScope = true
			}
		}
		if !inScope {
			continue
		}
		if h, held := occ[inst.ID]; held {
			count[h.releaseID]++
		}
	}
	if len(count) == 0 {
		return ""
	}
	ids := make([]string, 0, len(count))
	for id := range count {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s 仍占用 %d 台", id, count[id]))
	}
	return "（" + strings.Join(parts, "，") + "）"
}

// batchDemand counts how many instances of the batch fall into each zone.
func batchDemand(tx *store.Tx, b *store.Batch) map[string]int {
	demand := map[string]int{}
	for _, m := range b.Members {
		inst := findInstance(tx, m.InstanceID)
		if inst == nil {
			continue
		}
		demand[inst.ZoneID]++
	}
	return demand
}

// checkBatchCapacity decides whether starting this batch keeps every
// user-agreed floor: per-service-per-zone policies and shared pools. It
// always returns the full basis (for the persisted preflight) plus the
// list of violated floors when the batch must wait.
func checkBatchCapacity(tx *store.Tx, rel *store.Release, b *store.Batch) (bool, []string, []string) {
	occ := occupancyMap(tx)
	stats := zoneStats(tx, occ)
	demand := batchDemand(tx, b)
	svcName := serviceName(tx, rel.ServiceID)
	var basis, blockers []string

	for _, p := range tx.Policies() {
		if p.ServiceID != rel.ServiceID {
			continue
		}
		d := demand[p.ZoneID]
		st := statFor(stats, p.ServiceID, p.ZoneID)
		after := st.available - d
		line := fmt.Sprintf("容量：%s @ %s 本批占用 %d 台，占用后可用 %d（下限 %d）",
			svcName, zoneName(tx, p.ZoneID), d, after, p.MinAvailable)
		if after < p.MinAvailable {
			blockers = append(blockers, line+" 跌破下限"+holdersText(tx, occ, []string{p.ServiceID}, p.ZoneID))
		}
		basis = append(basis, line)
	}
	for _, pl := range tx.Pools() {
		if !containsString(pl.ServiceIDs, rel.ServiceID) {
			continue
		}
		d := demand[pl.ZoneID]
		avail := 0
		for _, s := range pl.ServiceIDs {
			avail += statFor(stats, s, pl.ZoneID).available
		}
		after := avail - d
		line := fmt.Sprintf("共享容量：%s @ %s 本批占用 %d 台，占用后可用 %d（下限 %d）",
			pl.Name, zoneName(tx, pl.ZoneID), d, after, pl.MinAvailable)
		if after < pl.MinAvailable {
			blockers = append(blockers, line+" 跌破下限"+holdersText(tx, occ, pl.ServiceIDs, pl.ZoneID))
		}
		basis = append(basis, line)
	}
	return len(blockers) == 0, basis, blockers
}

// validatePlanCapacity rejects a release plan whose batches could never
// fit, even with zero concurrent occupancy: scheduling it would guarantee
// a zone drops below its floor, so the plan is refused up front with the
// exact capacity condition explained.
func validatePlanCapacity(tx *store.Tx, serviceID string, groups [][]string) error {
	svcName := serviceName(tx, serviceID)
	for gi, group := range groups {
		demand := map[string]int{}
		for _, instID := range group {
			inst := findInstance(tx, instID)
			if inst == nil {
				continue
			}
			demand[inst.ZoneID]++
		}
		for _, p := range tx.Policies() {
			if p.ServiceID != serviceID {
				continue
			}
			total := 0
			for _, inst := range tx.Instances() {
				if inst.ServiceID == serviceID && inst.ZoneID == p.ZoneID {
					total++
				}
			}
			if total-demand[p.ZoneID] < p.MinAvailable {
				return fmt.Errorf("第 %d 批在任何时刻都会跌破容量下限：%s @ %s 共 %d 台，本批占用 %d 台，下限 %d 台；请缩小每批实例数或改用分散区域策略",
					gi+1, svcName, zoneName(tx, p.ZoneID), total, demand[p.ZoneID], p.MinAvailable)
			}
		}
		for _, pl := range tx.Pools() {
			if !containsString(pl.ServiceIDs, serviceID) {
				continue
			}
			total := 0
			for _, inst := range tx.Instances() {
				if inst.ZoneID != pl.ZoneID {
					continue
				}
				for _, s := range pl.ServiceIDs {
					if inst.ServiceID == s {
						total++
					}
				}
			}
			if total-demand[pl.ZoneID] < pl.MinAvailable {
				return fmt.Errorf("第 %d 批在任何时刻都会跌破共享容量下限：%s @ %s 共 %d 台，本批占用 %d 台，下限 %d 台；请缩小每批实例数或改用分散区域策略",
					gi+1, pl.Name, zoneName(tx, pl.ZoneID), total, demand[pl.ZoneID], pl.MinAvailable)
			}
		}
	}
	return nil
}

// orderForStrategy returns the instance order used to chunk batches.
// zone_spread interleaves zones so one batch never drains a single zone.
func orderForStrategy(tx *store.Tx, insts []store.Instance, strategy string) []store.Instance {
	if strategy != "zone_spread" {
		return insts
	}
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
	for {
		progressed := false
		for _, z := range zones {
			if len(byZone[z]) == 0 {
				continue
			}
			out = append(out, byZone[z][0])
			byZone[z] = byZone[z][1:]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return out
}

// planBatches chunks ordered instance IDs into release batches.
func planBatches(ordered []store.Instance, strategy string, size int) [][]string {
	if size < 1 {
		size = 1
	}
	if strategy == "all_at_once" {
		size = len(ordered)
	}
	var groups [][]string
	for start := 0; start < len(ordered); start += size {
		end := start + size
		if end > len(ordered) {
			end = len(ordered)
		}
		var group []string
		for _, inst := range ordered[start:end] {
			group = append(group, inst.ID)
		}
		groups = append(groups, group)
	}
	return groups
}

// BuildCapacityView assembles the queryable capacity ledger: per
// service+zone floors, shared pools, and every commitment still held by
// prepared-but-not-yet-effective batches.
func BuildCapacityView(tx *store.Tx) store.CapacityView {
	occ := occupancyMap(tx)
	stats := zoneStats(tx, occ)
	view := store.CapacityView{
		Services:    []store.ZoneServiceCapacity{},
		Pools:       []store.PoolCapacity{},
		Commitments: []store.CapacityCommitment{},
	}

	keys := make([]zoneServiceKey, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].zoneID != keys[j].zoneID {
			return keys[i].zoneID < keys[j].zoneID
		}
		return keys[i].serviceID < keys[j].serviceID
	})
	floorOf := func(serviceID, zoneID string) (int, bool) {
		floor := 0
		has := false
		for _, p := range tx.Policies() {
			if p.ServiceID == serviceID && p.ZoneID == zoneID {
				has = true
				if p.MinAvailable > floor {
					floor = p.MinAvailable
				}
			}
		}
		return floor, has
	}
	for _, k := range keys {
		st := stats[k]
		floor, has := floorOf(k.serviceID, k.zoneID)
		z := zoneByID(tx, k.zoneID)
		row := store.ZoneServiceCapacity{
			ZoneID: k.zoneID, ZoneName: zoneName(tx, k.zoneID),
			ServiceID: k.serviceID, ServiceName: serviceName(tx, k.serviceID),
			Total: st.total, Occupied: st.occupied, Unavailable: st.unavailable,
			Available: st.available, MinAvailable: floor, HasPolicy: has,
			FloorOK: !has || st.available >= floor,
		}
		if z != nil {
			row.ZoneFailed = z.Failed
		}
		view.Services = append(view.Services, row)
	}

	for _, pl := range tx.Pools() {
		row := store.PoolCapacity{
			PoolID: pl.ID, Name: pl.Name, ZoneID: pl.ZoneID,
			ZoneName: zoneName(tx, pl.ZoneID), MinAvailable: pl.MinAvailable,
		}
		for _, s := range pl.ServiceIDs {
			st := statFor(stats, s, pl.ZoneID)
			row.Total += st.total
			row.Occupied += st.occupied
			row.Available += st.available
			row.ServiceNames = append(row.ServiceNames, serviceName(tx, s))
		}
		row.FloorOK = row.Available >= pl.MinAvailable
		view.Pools = append(view.Pools, row)
	}

	// Commitments: group occupied instances by (batch, zone).
	type ck struct {
		batchID string
		zoneID  string
	}
	grouped := map[ck][]string{}
	meta := map[ck]holder{}
	for instID, h := range occ {
		inst := findInstance(tx, instID)
		if inst == nil {
			continue
		}
		k := ck{h.batchID, inst.ZoneID}
		grouped[k] = append(grouped[k], instID)
		meta[k] = h
	}
	cks := make([]ck, 0, len(grouped))
	for k := range grouped {
		cks = append(cks, k)
	}
	sort.Slice(cks, func(i, j int) bool {
		if meta[cks[i]].releaseID != meta[cks[j]].releaseID {
			return meta[cks[i]].releaseID < meta[cks[j]].releaseID
		}
		if meta[cks[i]].index != meta[cks[j]].index {
			return meta[cks[i]].index < meta[cks[j]].index
		}
		return cks[i].zoneID < cks[j].zoneID
	})
	for _, k := range cks {
		h := meta[k]
		b := findBatch(tx, k.batchID)
		if b == nil {
			continue
		}
		ids := grouped[k]
		sort.Strings(ids)
		svcID := ""
		if rel := findRelease(tx, h.releaseID); rel != nil {
			svcID = rel.ServiceID
		}
		view.Commitments = append(view.Commitments, store.CapacityCommitment{
			ReleaseID: h.releaseID, BatchID: k.batchID, BatchIndex: h.index,
			ServiceID: svcID, ServiceName: serviceName(tx, svcID),
			ZoneID: k.zoneID, ZoneName: zoneName(tx, k.zoneID),
			InstanceIDs: ids, State: b.Phase,
		})
	}
	return view
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
