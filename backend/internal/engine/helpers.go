package engine

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"releasectl/internal/store"
)

var idCounter int64

func newID(tx *store.Tx, prefix string) string {
	n := atomic.AddInt64(&idCounter, 1)
	return fmt.Sprintf("%s-%d", prefix, n)
}

// ResetIDSequence is used by deterministic tests.
func ResetIDSequence() { atomic.StoreInt64(&idCounter, 0) }

func atomicLoad() int64 { return atomic.LoadInt64(&idCounter) }

func atomicBump(n int64) { atomic.StoreInt64(&idCounter, n) }

func nowStr(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func findRelease(tx *store.Tx, id string) *store.Release {
	for i := range tx.Releases() {
		if tx.Releases()[i].ID == id {
			return &tx.Releases()[i]
		}
	}
	return nil
}

func findBatch(tx *store.Tx, id string) *store.Batch {
	for i := range tx.Batches() {
		if tx.Batches()[i].ID == id {
			return &tx.Batches()[i]
		}
	}
	return nil
}

func findMember(tx *store.Tx, batchID, instanceID string) (*store.Batch, int) {
	b := findBatch(tx, batchID)
	if b == nil {
		return nil, -1
	}
	for i := range b.Members {
		if b.Members[i].InstanceID == instanceID {
			return b, i
		}
	}
	return b, -1
}

func findInstance(tx *store.Tx, id string) *store.Instance {
	for i := range tx.Instances() {
		if tx.Instances()[i].ID == id {
			return &tx.Instances()[i]
		}
	}
	return nil
}

func findService(tx *store.Tx, id string) *store.Service {
	for i := range tx.Services() {
		if tx.Services()[i].ID == id {
			return &tx.Services()[i]
		}
	}
	return nil
}

func serviceName(tx *store.Tx, id string) string {
	if s := findService(tx, id); s != nil {
		return s.Name
	}
	return id
}

// serviceInstances lists instances of one service in stable order.
func serviceInstances(tx *store.Tx, serviceID string) []store.Instance {
	var out []store.Instance
	for _, inst := range tx.Instances() {
		if inst.ServiceID == serviceID {
			out = append(out, inst)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// compareVersions orders semver-ish ids (v1.2.3); falls back to string order.
func compareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea != nil || eb != nil {
			if c := strings.Compare(a, b); c != 0 {
				return c
			}
			return 0
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(pa) < len(pb):
		return -1
	case len(pa) > len(pb):
		return 1
	}
	return 0
}

// evaluateCompat checks every rule whose provider is serviceID against a
// proposed target version. Findings are deterministic (sorted by rule id) so
// the persisted preflight basis is stable.
func evaluateCompat(tx *store.Tx, serviceID, targetVersion string) []store.CompatFinding {
	return evaluateCompatWith(tx, serviceID, targetVersion, nil)
}

// evaluateCompatWith is evaluateCompat with an optional simulated version
// override (serviceID -> version, "" means mixed/migrating). Plan
// comparison uses it to explain what would block a hypothetical step
// without touching any real state.
func evaluateCompatWith(tx *store.Tx, serviceID, targetVersion string, sim map[string]string) []store.CompatFinding {
	var rules []store.CompatRule
	for _, r := range tx.Rules() {
		if r.ProviderID == serviceID {
			rules = append(rules, r)
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	var out []store.CompatFinding
	for _, r := range rules {
		f := store.CompatFinding{
			RuleID: r.ID, ProviderID: r.ProviderID, DependentID: r.DependentID, Mode: r.Mode,
		}
		depInsts := serviceInstances(tx, r.DependentID)
		if len(depInsts) == 0 {
			f.Compatible = true
			f.Detail = fmt.Sprintf("依赖者 %s 暂无实例，约束暂不评估", serviceName(tx, r.DependentID))
			out = append(out, f)
			continue
		}
		dv := depInsts[0].CurrentVersion
		uniform := true
		for _, inst := range depInsts[1:] {
			if inst.CurrentVersion != dv {
				uniform = false
			}
		}
		if sim != nil {
			dv = sim[r.DependentID]
			uniform = dv != ""
		}
		if !uniform {
			f.Compatible = true
			f.Detail = fmt.Sprintf("依赖者 %s 实例版本不一致，按迁移中处理，仅提示", serviceName(tx, r.DependentID))
			out = append(out, f)
			continue
		}
		if r.DependentVersion != "" && r.DependentVersion != dv {
			f.Compatible = true
			f.Detail = fmt.Sprintf("依赖者 %s 已运行 %s，约束（针对 %s）不再适用",
				serviceName(tx, r.DependentID), dv, r.DependentVersion)
			out = append(out, f)
			continue
		}
		inRange := true
		if r.MinProviderVersion != "" && compareVersions(targetVersion, r.MinProviderVersion) < 0 {
			inRange = false
		}
		if r.MaxProviderVersion != "" && compareVersions(targetVersion, r.MaxProviderVersion) > 0 {
			inRange = false
		}
		dep := serviceName(tx, r.DependentID)
		if inRange {
			f.Compatible = true
			f.Detail = fmt.Sprintf("依赖者 %s（%s）兼容目标版本 %s", dep, dv, targetVersion)
			if r.Mode == "stay_old" {
				f.Detail = fmt.Sprintf("依赖者 %s 保持旧版 %s 仍兼容目标版本 %s", dep, dv, targetVersion)
			}
			out = append(out, f)
			continue
		}
		f.Compatible = false
		switch r.Mode {
		case "update_first":
			f.Blocking = true
			f.Detail = fmt.Sprintf("依赖者 %s 仍为 %s，仅兼容 %s %s~%s：必须先更新依赖者，才能推进到 %s",
				dep, dv, serviceName(tx, r.ProviderID), r.MinProviderVersion, r.MaxProviderVersion, targetVersion)
		case "pause":
			f.Detail = fmt.Sprintf("依赖者 %s（%s）尚未验证与 %s 的兼容性，需人工审查后推进",
				dep, dv, targetVersion)
		default: // stay_old
			f.Compatible = true
			f.Detail = fmt.Sprintf("依赖者 %s 保持旧版 %s，不随本次发布推进", dep, dv)
		}
		if r.Note != "" {
			f.Detail += "（" + r.Note + "）"
		}
		out = append(out, f)
	}
	return out
}

func hasBlockingFinding(findings []store.CompatFinding) bool {
	for _, f := range findings {
		if f.Blocking && !f.Compatible {
			return true
		}
	}
	return false
}

func hasPausingFinding(findings []store.CompatFinding) bool {
	for _, f := range findings {
		if !f.Compatible && f.Mode == "pause" {
			return true
		}
	}
	return false
}

// healthBasis describes the current health/version evidence of the batch
// members, recorded before the batch takes effect.
func healthBasis(tx *store.Tx, b *store.Batch) []string {
	var out []string
	for _, m := range b.Members {
		inst := findInstance(tx, m.InstanceID)
		if inst == nil {
			out = append(out, fmt.Sprintf("%s：实例记录缺失", m.InstanceID))
			continue
		}
		out = append(out, fmt.Sprintf("%s 当前 %s，健康状态 %s", inst.ID, inst.CurrentVersion, healthText(inst.Health)))
	}
	return out
}

func healthText(h string) string {
	switch h {
	case "healthy":
		return "健康"
	case "unhealthy":
		return "不健康"
	default:
		return "未知"
	}
}

func releaseBatches(tx *store.Tx, releaseID string) []*store.Batch {
	var out []*store.Batch
	for i := range tx.Batches() {
		if tx.Batches()[i].ReleaseID == releaseID {
			out = append(out, &tx.Batches()[i])
		}
	}
	return out
}

func inAnyBatch(batches []*store.Batch, instanceID string) bool {
	for _, b := range batches {
		for _, m := range b.Members {
			if m.InstanceID == instanceID {
				return true
			}
		}
	}
	return false
}

// rebuildPendingBatches replaces every not-yet-started batch of a release
// with smaller batches over the instances that no started batch has touched.
// Finished batches and the currently paused batch are preserved exactly, so a
// reopen followed by "shrink" never re-executes work that already happened.
func rebuildPendingBatches(tx *store.Tx, rel *store.Release, size int, now time.Time) error {
	old := releaseBatches(tx, rel.ID)
	sortBatches(old)
	started := map[string]bool{}
	maxIndex := -1
	var remaining []string
	for _, b := range old {
		if b.Phase != "pending" {
			maxIndex = b.Index
			for _, m := range b.Members {
				started[m.InstanceID] = true
			}
		}
	}
	for _, b := range old {
		if b.Phase != "pending" {
			continue
		}
		for _, m := range b.Members {
			if !started[m.InstanceID] {
				remaining = append(remaining, m.InstanceID)
			}
		}
	}
	// Keep the release's ordering strategy when re-chunking, so a
	// zone-spread release stays zone-spread after a shrink.
	if rel.Strategy == "zone_spread" {
		var insts []store.Instance
		for _, id := range remaining {
			if inst := findInstance(tx, id); inst != nil {
				insts = append(insts, *inst)
			}
		}
		remaining = remaining[:0]
		for _, inst := range orderForStrategy(tx, insts, rel.Strategy) {
			remaining = append(remaining, inst.ID)
		}
	}
	kept := tx.Batches()[:0]
	for _, b := range tx.Batches() {
		if b.ReleaseID == rel.ID && b.Phase == "pending" {
			continue
		}
		kept = append(kept, b)
	}
	// Pending batches never have ops, but drop any stale ones defensively.
	ops := tx.Ops()[:0]
	pendingIDs := map[string]bool{}
	for _, b := range old {
		if b.Phase == "pending" {
			pendingIDs[b.ID] = true
		}
	}
	for _, op := range tx.Ops() {
		if !pendingIDs[op.BatchID] {
			ops = append(ops, op)
		}
	}
	tx.SetOps(ops)

	idx := maxIndex + 1
	batchIDs := rel.BatchIDs[:0]
	for _, b := range kept {
		if b.ReleaseID == rel.ID {
			batchIDs = append(batchIDs, b.ID)
		}
	}
	for start := 0; start < len(remaining); start += size {
		end := start + size
		if end > len(remaining) {
			end = len(remaining)
		}
		b := store.Batch{
			ID: newID(tx, "batch"), ReleaseID: rel.ID, Index: idx,
			Phase: "pending", TargetVersion: rel.TargetVersion,
		}
		for _, id := range remaining[start:end] {
			inst := findInstance(tx, id)
			from := rel.Baseline
			if inst != nil {
				from = inst.CurrentVersion
			}
			b.Members = append(b.Members, store.MemberState{
				InstanceID: id, FromVersion: from, ToVersion: rel.TargetVersion,
				UpdateState: "pending", HealthState: "pending",
			})
		}
		kept = append(kept, b)
		batchIDs = append(batchIDs, b.ID)
		idx++
	}
	rel.BatchIDs = append([]string(nil), batchIDs...)
	tx.SetBatches(kept)
	return nil
}

func sortBatches(bs []*store.Batch) {
	sort.Slice(bs, func(i, j int) bool { return bs[i].Index < bs[j].Index })
}

// rejectOnFailureEvidence blocks "continue"/"shrink" when the active batch
// already contains a failed update or an unhealthy observation: such a batch
// can only be rolled back or explicitly ended.
func rejectOnFailureEvidence(tx *store.Tx, rel *store.Release, msg string) error {
	for _, b := range releaseBatches(tx, rel.ID) {
		if b.Phase != "paused" {
			continue
		}
		for _, m := range b.Members {
			if m.UpdateState == "failed" || m.HealthState == "unhealthy" {
				return errors.New(msg)
			}
		}
	}
	return nil
}

func batchLabel(b *store.Batch) string {
	return fmt.Sprintf("第 %d 批", b.Index+1)
}

func healthVerdictText(v string) string {
	if v == "healthy" {
		return "健康"
	}
	return "不健康"
}

// updateShouldFail decides the virtual outcome of an update op.
func updateShouldFail(rel store.Release, b store.Batch, m store.MemberState) bool {
	switch rel.Failure.Mode {
	case "instances":
		for _, id := range rel.Failure.Instances {
			if id == m.InstanceID {
				return true
			}
		}
	}
	return false
}

// healthShouldFail realizes the half-batch failure drill: within the batch
// that contains the first listed failing instance, its first half fails
// health observation. Explicit instance listing also works.
func healthShouldFail(rel store.Release, b store.Batch, m store.MemberState) bool {
	switch rel.Failure.Mode {
	case "half_batch":
		if len(b.Members) == 0 {
			return false
		}
		half := len(b.Members) / 2
		if half < 1 {
			half = 1
		}
		for i := 0; i < half; i++ {
			if b.Members[i].InstanceID == m.InstanceID {
				return true
			}
		}
	case "instances":
		for _, id := range rel.Failure.Instances {
			if id == m.InstanceID {
				return true
			}
		}
	}
	return false
}
