package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"releasectl/internal/store"
)

// CapacityInstanceView is one instance's occupancy inside a grant. The same
// instance appearing under different target versions (different batches or
// releases) is reported as separate entries keyed by version.
type CapacityInstanceView struct {
	InstanceID   string `json:"instanceId"`
	FromVersion  string `json:"fromVersion"`
	ToVersion    string `json:"toVersion"`
	Released     bool   `json:"released"`
	ReleasedAt   string `json:"releasedAt,omitempty"`
	ReleaseOrder int    `json:"releaseOrder,omitempty"`
	ReleaseWhy   string `json:"releaseWhy,omitempty"`
}

// CapacityGrantView is one capacity commitment with its occupancy window.
type CapacityGrantView struct {
	ID         string                 `json:"id"`
	BatchID    string                 `json:"batchId"`
	BatchIndex int                    `json:"batchIndex"`
	ZoneID     string                 `json:"zoneId"`
	ZoneName   string                 `json:"zoneName"`
	State      string                 `json:"state"`
	AcquiredAt string                 `json:"acquiredAt"`
	ReleasedAt string                 `json:"releasedAt,omitempty"`
	ReleaseWhy string                 `json:"releaseWhy,omitempty"`
	Instances  []CapacityInstanceView `json:"instances"`
}

// CapacityWaitView is the persisted waiting period of one batch that could
// not acquire zone capacity immediately.
type CapacityWaitView struct {
	BatchID    string `json:"batchId"`
	BatchIndex int    `json:"batchIndex"`
	Waiting    bool   `json:"waiting"`
	Seconds    int64  `json:"seconds"`
	Reason     string `json:"reason"`
	Since      string `json:"since"`
	Until      string `json:"until,omitempty"`
}

// CapacityPolicyView is the minimum-available policy of one zone as it
// applies to the release's service, plus the persisted basis lines recorded
// when the batch acquired its commitment.
type CapacityPolicyView struct {
	ZoneID         string   `json:"zoneId"`
	ZoneName       string   `json:"zoneName"`
	HasMin         bool     `json:"hasMin"`
	MinAvailable   int      `json:"minAvailable"`
	SharedCapacity int      `json:"sharedCapacity"`
	Basis          []string `json:"basis"`
}

// ReleaseCapacityView is the full capacity footprint of one release.
type ReleaseCapacityView struct {
	ReleaseID     string               `json:"releaseId"`
	ServiceID     string               `json:"serviceId"`
	ServiceName   string               `json:"serviceName"`
	TargetVersion string               `json:"targetVersion"`
	Baseline      string               `json:"baseline"`
	Status        string               `json:"status"`
	CreatedAt     string               `json:"createdAt"`
	FinishedAt    string               `json:"finishedAt,omitempty"`
	Grants        []CapacityGrantView  `json:"grants"`
	Waits         []CapacityWaitView   `json:"waits"`
	Policies      []CapacityPolicyView `json:"policies"`
}

// CapacityComparison is the side-by-side payload for one or two releases.
type CapacityComparison struct {
	GeneratedAt string                `json:"generatedAt"`
	Releases    []ReleaseCapacityView `json:"releases"`
}

// CompareReleaseCapacity builds the capacity comparison for the given
// releases. It is strictly read-only: it runs inside Store.View, never
// acquires commitments, never releases instances and never touches release
// state, so viewing history or exporting cannot disturb an in-flight drill.
func (s *Service) CompareReleaseCapacity(ids []string, now time.Time) (*CapacityComparison, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("至少选择一个发布进行对比")
	}
	out := &CapacityComparison{GeneratedAt: nowStr(now)}
	var err error
	s.st.View(func(tx *store.Tx) {
		for _, id := range ids {
			var rel *store.Release
			for i := range tx.Releases() {
				if tx.Releases()[i].ID == id {
					rel = &tx.Releases()[i]
					break
				}
			}
			if rel == nil {
				err = fmt.Errorf("发布 %s 不存在", id)
				return
			}
			out.Releases = append(out.Releases, buildReleaseCapacityView(tx, rel, now))
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func buildReleaseCapacityView(tx *store.Tx, rel *store.Release, now time.Time) ReleaseCapacityView {
	view := ReleaseCapacityView{
		ReleaseID: rel.ID, ServiceID: rel.ServiceID, ServiceName: serviceName(tx, rel.ServiceID),
		TargetVersion: rel.TargetVersion, Baseline: rel.Baseline, Status: rel.Status,
		CreatedAt: rel.CreatedAt, FinishedAt: rel.FinishedAt,
		Grants: []CapacityGrantView{}, Waits: []CapacityWaitView{}, Policies: []CapacityPolicyView{},
	}
	batches := map[string]*store.Batch{}
	for i := range tx.Batches() {
		b := &tx.Batches()[i]
		if b.ReleaseID == rel.ID {
			batches[b.ID] = b
		}
	}

	// Per-instance release evidence from the persisted event log: each
	// settled instance produced an event when its slot was handed back.
	type instRelease struct {
		at  string
		seq int64
	}
	releasedEv := map[string]instRelease{} // batchID|instanceID -> event
	for _, e := range tx.Events() {
		if e.ReleaseID != rel.ID || e.InstanceID == "" {
			continue
		}
		if !strings.Contains(e.Message, "容量占用释放") {
			continue
		}
		key := e.BatchID + "|" + e.InstanceID
		if _, seen := releasedEv[key]; !seen {
			releasedEv[key] = instRelease{at: e.At, seq: e.Seq}
		}
	}
	// Global per-release return order, sorted by event sequence.
	type orderEntry struct {
		key string
		seq int64
	}
	var order []orderEntry
	for k, v := range releasedEv {
		order = append(order, orderEntry{key: k, seq: v.seq})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].seq < order[j].seq })
	orderOf := map[string]int{}
	for i, o := range order {
		orderOf[o.key] = i + 1
	}

	grants := append([]store.CapacityGrant{}, tx.Grants()...)
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].AcquiredAt != grants[j].AcquiredAt {
			return grants[i].AcquiredAt < grants[j].AcquiredAt
		}
		return grants[i].ID < grants[j].ID
	})
	zoneTouched := map[string]bool{}
	for _, g := range grants {
		if g.ReleaseID != rel.ID {
			continue
		}
		b := batches[g.BatchID]
		gv := CapacityGrantView{
			ID: g.ID, BatchID: g.BatchID, ZoneID: g.ZoneID, ZoneName: zoneName(tx, g.ZoneID),
			State: g.State, AcquiredAt: g.AcquiredAt, ReleasedAt: g.ReleasedAt,
			ReleaseWhy: g.ReleaseWhy, Instances: []CapacityInstanceView{},
		}
		if b != nil {
			gv.BatchIndex = b.Index
		}
		zoneTouched[g.ZoneID] = true
		stillHeld := map[string]bool{}
		for _, id := range g.InstanceIDs {
			stillHeld[id] = true
		}
		// One row per (instance, version): members of the grant's batch that
		// belong to this zone. Instances settled earlier are already dropped
		// from g.InstanceIDs, so iterate the batch members instead.
		if b != nil {
			for _, m := range b.Members {
				inst := findInstance(tx, m.InstanceID)
				zid := ""
				if inst != nil {
					zid = inst.ZoneID
				}
				if zid != g.ZoneID {
					continue
				}
				iv := CapacityInstanceView{
					InstanceID: m.InstanceID, FromVersion: m.FromVersion, ToVersion: m.ToVersion,
				}
				key := g.BatchID + "|" + m.InstanceID
				if stillHeld[m.InstanceID] {
					iv.Released = false
				} else if ev, ok := releasedEv[key]; ok {
					iv.Released = true
					iv.ReleasedAt = ev.at
					iv.ReleaseOrder = orderOf[key]
					iv.ReleaseWhy = "实例归宿已确认"
				} else if g.State == "released" {
					iv.Released = true
					iv.ReleasedAt = g.ReleasedAt
					iv.ReleaseWhy = g.ReleaseWhy
				}
				gv.Instances = append(gv.Instances, iv)
			}
		}
		sort.Slice(gv.Instances, func(i, j int) bool {
			a, bb := gv.Instances[i], gv.Instances[j]
			if a.ReleaseOrder != bb.ReleaseOrder {
				if a.ReleaseOrder == 0 {
					return false
				}
				if bb.ReleaseOrder == 0 {
					return true
				}
				return a.ReleaseOrder < bb.ReleaseOrder
			}
			return a.InstanceID < bb.InstanceID
		})
		view.Grants = append(view.Grants, gv)
	}

	// Waiting periods: from the first persisted "等待区域容量" event of a
	// batch to its "取得容量承诺" event (or to now while still waiting).
	acquiredAt := map[string]string{}
	waitSince := map[string]string{}
	waitReason := map[string]string{}
	for _, e := range tx.Events() {
		if e.ReleaseID != rel.ID || e.BatchID == "" {
			continue
		}
		if strings.Contains(e.Message, "取得故障区域") && strings.Contains(e.Message, "容量承诺") {
			if _, seen := acquiredAt[e.BatchID]; !seen {
				acquiredAt[e.BatchID] = e.At
			}
		}
		if strings.Contains(e.Message, "等待区域容量") {
			if _, seen := waitSince[e.BatchID]; !seen {
				waitSince[e.BatchID] = e.At
				waitReason[e.BatchID] = e.Message
			}
		}
	}
	var waitBatches []*store.Batch
	for _, b := range batches {
		waitBatches = append(waitBatches, b)
	}
	sort.Slice(waitBatches, func(i, j int) bool { return waitBatches[i].Index < waitBatches[j].Index })
	for _, b := range waitBatches {
		since, waited := waitSince[b.ID]
		if !waited && b.CapacityWait == "" {
			continue
		}
		wv := CapacityWaitView{
			BatchID: b.ID, BatchIndex: b.Index, Since: since,
			Reason: waitReason[b.ID],
		}
		if b.CapacityWait != "" {
			wv.Waiting = true
			wv.Reason = b.CapacityWait
			if wv.Since == "" {
				wv.Since = nowStr(now)
			}
			wv.Seconds = secondsBetween(wv.Since, nowStr(now))
		} else if until, ok := acquiredAt[b.ID]; ok {
			wv.Until = until
			wv.Seconds = secondsBetween(since, until)
		}
		view.Waits = append(view.Waits, wv)
	}

	// Minimum-available policy per touched zone, plus the persisted basis
	// lines recorded when each batch acquired its commitment.
	var zoneIDs []string
	for zid := range zoneTouched {
		zoneIDs = append(zoneIDs, zid)
	}
	sort.Strings(zoneIDs)
	for _, zid := range zoneIDs {
		z := zoneByID(tx, zid)
		pv := CapacityPolicyView{ZoneID: zid, ZoneName: zoneName(tx, zid), Basis: []string{}}
		if z != nil {
			pv.SharedCapacity = z.SharedCapacity
			if min, ok := z.MinAvailable[rel.ServiceID]; ok {
				pv.HasMin = true
				pv.MinAvailable = min
			}
		}
		seenBasis := map[string]bool{}
		for _, b := range waitBatches {
			if b.Preflight == nil {
				continue
			}
			for _, line := range b.Preflight.CapacityBasis {
				if !strings.Contains(line, pv.ZoneName) || seenBasis[line] {
					continue
				}
				seenBasis[line] = true
				pv.Basis = append(pv.Basis, line)
			}
		}
		view.Policies = append(view.Policies, pv)
	}
	return view
}

func secondsBetween(from, to string) int64 {
	f, err1 := time.Parse(time.RFC3339Nano, from)
	t, err2 := time.Parse(time.RFC3339Nano, to)
	if err1 != nil || err2 != nil || t.Before(f) {
		return 0
	}
	return int64(t.Sub(f).Seconds())
}
