package engine

import (
	"sort"
	"strings"
	"time"

	"releasectl/internal/store"
)

// CapacityCompareFilters selects which releases appear in the cross-release
// capacity comparison. All fields are optional.
type CapacityCompareFilters struct {
	ZoneID    string
	ServiceID string
	Status    string // "" = all, "ended" = any terminal status, or an exact status
	From      string // release created at or after (RFC3339 or YYYY-MM-DD)
	To        string // release created at or before
}

// GrantWindow is one capacity commitment's occupancy window.
type GrantWindow struct {
	GrantID       string   `json:"grantId"`
	BatchID       string   `json:"batchId"`
	BatchIndex    int      `json:"batchIndex"`
	ZoneID        string   `json:"zoneId"`
	ZoneName      string   `json:"zoneName"`
	State         string   `json:"state"`
	Instances     []string `json:"instances"`
	AcquiredAt    string   `json:"acquiredAt"`
	ReleasedAt    string   `json:"releasedAt,omitempty"`
	ReleaseWhy    string   `json:"releaseWhy,omitempty"`
	WaitSeconds   float64  `json:"waitSeconds"`
	OccupySeconds float64  `json:"occupySeconds"`
}

// InstanceReturn is one instance's capacity release, in persisted order.
type InstanceReturn struct {
	Order      int    `json:"order"`
	InstanceID string `json:"instanceId"`
	ZoneName   string `json:"zoneName"`
	At         string `json:"at"`
	Why        string `json:"why"`
}

// InstanceOccupancy ties an occupied instance to the exact version
// transition it was held for, so the same instance occupied under different
// target versions shows up as separate rows.
type InstanceOccupancy struct {
	InstanceID  string `json:"instanceId"`
	FromVersion string `json:"fromVersion"`
	ToVersion   string `json:"toVersion"`
	BatchIndex  int    `json:"batchIndex"`
	ZoneName    string `json:"zoneName"`
}

// ZonePolicy is the minimum-available / shared-capacity policy of one zone
// touched by the release, captured for side-by-side policy comparison.
type ZonePolicy struct {
	ZoneID         string `json:"zoneId"`
	ZoneName       string `json:"zoneName"`
	MinAvailable   int    `json:"minAvailable"`
	SharedCapacity int    `json:"sharedCapacity"`
}

// WaitingBatch is a batch still blocked on zone capacity right now.
type WaitingBatch struct {
	BatchID    string `json:"batchId"`
	BatchIndex int    `json:"batchIndex"`
	Reason     string `json:"reason"`
	Since      string `json:"since"`
}

// ReleaseCapacity is the full capacity footprint of one release.
type ReleaseCapacity struct {
	ReleaseID       string              `json:"releaseId"`
	ServiceID       string              `json:"serviceId"`
	ServiceName     string              `json:"serviceName"`
	TargetVersion   string              `json:"targetVersion"`
	Baseline        string              `json:"baseline"`
	Status          string              `json:"status"`
	CreatedAt       string              `json:"createdAt"`
	FinishedAt      string              `json:"finishedAt,omitempty"`
	Grants          []GrantWindow       `json:"grants"`
	Returns         []InstanceReturn    `json:"returns"`
	Occupancy       []InstanceOccupancy `json:"occupancy"`
	Policies        []ZonePolicy        `json:"policies"`
	Waiting         []WaitingBatch      `json:"waiting"`
	TotalWaitSecond float64             `json:"totalWaitSeconds"`
}

// CapacityCompareResult is the read-only comparison payload.
type CapacityCompareResult struct {
	GeneratedAt string            `json:"generatedAt"`
	Releases    []ReleaseCapacity `json:"releases"`
}

var terminalStatuses = map[string]bool{
	"succeeded": true, "failed": true, "rolled_back": true, "cancelled": true,
}

func parseFilterTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// BuildCapacityCompare assembles the cross-release capacity comparison from
// persisted records only. It never mutates the store: viewing history or
// exporting can never acquire commitments, release instances, or change any
// release state.
func BuildCapacityCompare(tx *store.Tx, f CapacityCompareFilters, now time.Time) CapacityCompareResult {
	from, hasFrom := parseFilterTime(f.From)
	to, hasTo := parseFilterTime(f.To)
	zones := map[string]store.Zone{}
	for _, z := range tx.Zones() {
		zones[z.ID] = z
	}
	svcNames := map[string]string{}
	for _, s := range tx.Services() {
		svcNames[s.ID] = s.Name
	}
	instZone := map[string]string{}
	for _, inst := range tx.Instances() {
		instZone[inst.ID] = inst.ZoneID
	}
	batchByID := map[string]*store.Batch{}
	batchesByRelease := map[string][]store.Batch{}
	for i := range tx.Batches() {
		b := tx.Batches()[i]
		batchByID[b.ID] = &b
		batchesByRelease[b.ReleaseID] = append(batchesByRelease[b.ReleaseID], b)
	}
	grantsByRelease := map[string][]store.CapacityGrant{}
	for _, g := range tx.Grants() {
		grantsByRelease[g.ReleaseID] = append(grantsByRelease[g.ReleaseID], g)
	}
	// Per-instance capacity release events, in persisted sequence order.
	returnsByRelease := map[string][]InstanceReturn{}
	for _, ev := range tx.Events() {
		if ev.InstanceID == "" || ev.ReleaseID == "" {
			continue
		}
		if !strings.Contains(ev.Message, "容量占用释放") {
			continue
		}
		list := returnsByRelease[ev.ReleaseID]
		why := ""
		if i := strings.Index(ev.Message, "："); i >= 0 {
			why = ev.Message[i+len("："):]
		}
		zoneName := ""
		if zid := instZone[ev.InstanceID]; zid != "" {
			if z, ok := zones[zid]; ok {
				zoneName = z.Name
			}
		}
		returnsByRelease[ev.ReleaseID] = append(list, InstanceReturn{
			Order: len(list) + 1, InstanceID: ev.InstanceID,
			ZoneName: zoneName, At: ev.At, Why: why,
		})
	}

	res := CapacityCompareResult{
		GeneratedAt: now.UTC().Format(time.RFC3339Nano),
		Releases:    []ReleaseCapacity{},
	}
	for _, rel := range tx.Releases() {
		if f.ServiceID != "" && rel.ServiceID != f.ServiceID {
			continue
		}
		if f.Status == "ended" {
			if !terminalStatuses[rel.Status] {
				continue
			}
		} else if f.Status != "" && rel.Status != f.Status {
			continue
		}
		if created, err := time.Parse(time.RFC3339Nano, rel.CreatedAt); err == nil {
			if hasFrom && created.Before(from) {
				continue
			}
			if hasTo && created.After(to) {
				continue
			}
		}
		grants := grantsByRelease[rel.ID]
		if f.ZoneID != "" {
			hit := false
			for _, g := range grants {
				if g.ZoneID == f.ZoneID {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}

		rc := ReleaseCapacity{
			ReleaseID: rel.ID, ServiceID: rel.ServiceID,
			ServiceName: svcNames[rel.ServiceID], TargetVersion: rel.TargetVersion,
			Baseline: rel.Baseline, Status: rel.Status, CreatedAt: rel.CreatedAt,
			FinishedAt: rel.FinishedAt,
			Grants:     []GrantWindow{}, Returns: returnsByRelease[rel.ID],
			Occupancy: []InstanceOccupancy{}, Policies: []ZonePolicy{},
			Waiting: []WaitingBatch{},
		}
		if rc.Returns == nil {
			rc.Returns = []InstanceReturn{}
		}
		policyZones := map[string]bool{}
		for _, g := range grants {
			b := batchByID[g.BatchID]
			gw := GrantWindow{
				GrantID: g.ID, BatchID: g.BatchID, ZoneID: g.ZoneID,
				State: g.State, Instances: append([]string{}, g.InstanceIDs...),
				AcquiredAt: g.AcquiredAt, ReleasedAt: g.ReleasedAt,
				ReleaseWhy: g.ReleaseWhy,
			}
			if z, ok := zones[g.ZoneID]; ok {
				gw.ZoneName = z.Name
			}
			if b != nil {
				gw.BatchIndex = b.Index
				if b.CapacityWaitSince != "" {
					if ws, err1 := time.Parse(time.RFC3339Nano, b.CapacityWaitSince); err1 == nil {
						if acq, err2 := time.Parse(time.RFC3339Nano, g.AcquiredAt); err2 == nil {
							gw.WaitSeconds = acq.Sub(ws).Seconds()
						}
					}
				}
			}
			if acq, err1 := time.Parse(time.RFC3339Nano, g.AcquiredAt); err1 == nil {
				end := now
				if g.ReleasedAt != "" {
					if rel, err2 := time.Parse(time.RFC3339Nano, g.ReleasedAt); err2 == nil {
						end = rel
					}
				}
				gw.OccupySeconds = end.Sub(acq).Seconds()
			}
			rc.TotalWaitSecond += gw.WaitSeconds
			rc.Grants = append(rc.Grants, gw)
			if !policyZones[g.ZoneID] {
				policyZones[g.ZoneID] = true
				if z, ok := zones[g.ZoneID]; ok {
					rc.Policies = append(rc.Policies, ZonePolicy{
						ZoneID: z.ID, ZoneName: z.Name,
						MinAvailable:   z.MinAvailable[rel.ServiceID],
						SharedCapacity: z.SharedCapacity,
					})
				}
			}
		}
		sort.Slice(rc.Grants, func(i, j int) bool {
			if rc.Grants[i].AcquiredAt != rc.Grants[j].AcquiredAt {
				return rc.Grants[i].AcquiredAt < rc.Grants[j].AcquiredAt
			}
			return rc.Grants[i].GrantID < rc.Grants[j].GrantID
		})
		sort.Slice(rc.Policies, func(i, j int) bool { return rc.Policies[i].ZoneID < rc.Policies[j].ZoneID })
		for _, b := range batchesByRelease[rel.ID] {
			if b.CapacityWait != "" {
				rc.Waiting = append(rc.Waiting, WaitingBatch{
					BatchID: b.ID, BatchIndex: b.Index,
					Reason: b.CapacityWait, Since: b.CapacityWaitSince,
				})
			}
			for _, m := range b.Members {
				zoneName := ""
				if zid := instZone[m.InstanceID]; zid != "" {
					if z, ok := zones[zid]; ok {
						zoneName = z.Name
					}
				}
				rc.Occupancy = append(rc.Occupancy, InstanceOccupancy{
					InstanceID: m.InstanceID, FromVersion: m.FromVersion,
					ToVersion: m.ToVersion, BatchIndex: b.Index, ZoneName: zoneName,
				})
			}
		}
		sort.Slice(rc.Occupancy, func(i, j int) bool {
			if rc.Occupancy[i].BatchIndex != rc.Occupancy[j].BatchIndex {
				return rc.Occupancy[i].BatchIndex < rc.Occupancy[j].BatchIndex
			}
			return rc.Occupancy[i].InstanceID < rc.Occupancy[j].InstanceID
		})
		res.Releases = append(res.Releases, rc)
	}
	return res
}
