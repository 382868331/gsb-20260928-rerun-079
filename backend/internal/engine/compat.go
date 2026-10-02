package engine

import (
	"fmt"
	"sort"
	"time"

	"releasectl/internal/store"
)

// serviceName resolves a service display name for messages.
func serviceName(tx *store.Tx, id string) string {
	for _, s := range tx.Services() {
		if s.ID == id {
			return s.Name
		}
	}
	return id
}

// serviceVersions returns the service's versions in creation order: the
// first is the baseline, the last the newest available target.
func serviceVersions(tx *store.Tx, serviceID string) []store.Version {
	var out []store.Version
	for _, v := range tx.Versions() {
		if v.ServiceID == serviceID {
			out = append(out, v)
		}
	}
	return out
}

func serviceInstances(tx *store.Tx, serviceID string) []store.Instance {
	var out []store.Instance
	for _, inst := range tx.Instances() {
		if inst.ServiceID == serviceID {
			out = append(out, inst)
		}
	}
	return out
}

// activeReleaseOf returns the non-terminal release of a service, if any.
// When excludePaused is set, a paused release does not count as "actively
// advancing" (that is exactly what the pause compat mode requires).
func activeReleaseOf(tx *store.Tx, serviceID string, excludePaused bool) *store.Release {
	for i := range tx.Releases() {
		r := &tx.Releases()[i]
		if r.ServiceID != serviceID || isReleaseTerminal(r.Status) {
			continue
		}
		if excludePaused && r.Status == "paused" {
			continue
		}
		return r
	}
	return nil
}

// advancingReleaseOf returns the service's release only while it is truly
// advancing: not terminal, not paused, and not stuck at the compatibility
// gate. A gate-blocked release has moved nothing, so it must not in turn
// block other services (that would deadlock two waiting releases).
func advancingReleaseOf(tx *store.Tx, serviceID string) *store.Release {
	r := activeReleaseOf(tx, serviceID, true)
	if r == nil {
		return nil
	}
	var active *store.Batch
	for i := range tx.Batches() {
		b := &tx.Batches()[i]
		if b.ReleaseID != r.ID || isBatchTerminal(b.Phase) {
			continue
		}
		if active == nil || b.Index < active.Index {
			active = b
		}
	}
	if active != nil && active.Phase == "pending" && active.BlockedReason != "" {
		return nil
	}
	return r
}

// evaluateCompat computes the compatibility and health evidence that must
// hold before a batch of rel may take effect. A batch is never allowed to
// expand just because its own members look healthy: every constraint that
// other services impose on this one (and vice versa) is checked first, and
// the verdicts are persisted on the batch as evidence.
func evaluateCompat(tx *store.Tx, rel *store.Release) (checks []store.CompatCheck, blocked []string) {
	svcID := rel.ServiceID
	related := map[string]bool{svcID: true}
	for _, rule := range tx.CompatRules() {
		if rule.DependencyID == svcID || rule.DependentID == svcID {
			related[rule.DependencyID] = true
			related[rule.DependentID] = true
		}
	}
	// Health basis: the release may only proceed while every related service
	// is free of unhealthy instances.
	ids := make([]string, 0, len(related))
	for id := range related {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		insts := serviceInstances(tx, id)
		healthy := 0
		for _, inst := range insts {
			if inst.Health == "healthy" {
				healthy++
			}
		}
		ok := healthy == len(insts)
		c := store.CompatCheck{ServiceID: id, Kind: "health", OK: ok,
			Detail: fmt.Sprintf("%s 健康依据：%d/%d 实例健康", serviceName(tx, id), healthy, len(insts))}
		if !ok {
			c.Detail += "，存在非健康实例，批次不得生效"
			blocked = append(blocked, c.Detail)
		}
		checks = append(checks, c)
	}
	// Compatibility constraints in both directions.
	for _, rule := range tx.CompatRules() {
		var c store.CompatCheck
		switch {
		case rule.DependencyID == svcID:
			// We are upgrading the dependency; the dependent must satisfy
			// the rule's mode before our batch may take effect.
			dep := rule.DependentID
			depName := serviceName(tx, dep)
			switch rule.Mode {
			case "dependent_first":
				vers := serviceVersions(tx, dep)
				latest := ""
				if len(vers) > 0 {
					latest = vers[len(vers)-1].ID
				}
				insts := serviceInstances(tx, dep)
				onLatest := 0
				for _, inst := range insts {
					if inst.CurrentVersion == latest {
						onLatest++
					}
				}
				c = store.CompatCheck{ServiceID: dep, Kind: "compat", OK: latest != "" && onLatest == len(insts),
					Detail: fmt.Sprintf("依赖方 %s 必须先完成更新：%d/%d 实例已在 %s", depName, onLatest, len(insts), latest)}
			case "stay_old":
				vers := serviceVersions(tx, dep)
				baseline := ""
				if len(vers) > 0 {
					baseline = vers[0].ID
				}
				insts := serviceInstances(tx, dep)
				onBase := 0
				for _, inst := range insts {
					if inst.CurrentVersion == baseline {
						onBase++
					}
				}
				rel2 := advancingReleaseOf(tx, dep)
				c = store.CompatCheck{ServiceID: dep, Kind: "compat", OK: onBase == len(insts) && rel2 == nil,
					Detail: fmt.Sprintf("依赖方 %s 必须保持旧版 %s 且无正在推进的发布", depName, baseline)}
			case "pause":
				rel2 := advancingReleaseOf(tx, dep)
				c = store.CompatCheck{ServiceID: dep, Kind: "compat", OK: rel2 == nil,
					Detail: fmt.Sprintf("依赖方 %s 的发布必须暂停推进（无正在推进的发布）", depName)}
			}
		case rule.DependentID == svcID:
			// We are the dependent; the dependency's state constrains us.
			dep := rule.DependencyID
			depName := serviceName(tx, dep)
			switch rule.Mode {
			case "dependent_first":
				c = store.CompatCheck{ServiceID: dep, Kind: "compat", OK: true,
					Detail: fmt.Sprintf("本服务先于 %s 更新，满足先后顺序约束", depName)}
			case "stay_old", "pause":
				rel2 := advancingReleaseOf(tx, dep)
				modeText := "本服务必须保持旧版"
				if rule.Mode == "pause" {
					modeText = "本服务必须暂停推进"
				}
				c = store.CompatCheck{ServiceID: dep, Kind: "compat", OK: rel2 == nil,
					Detail: fmt.Sprintf("%s 无进行中的发布（否则%s）", depName, modeText)}
			}
		default:
			continue
		}
		if !c.OK {
			c.Detail += "（未满足）"
			blocked = append(blocked, c.Detail)
		}
		checks = append(checks, c)
	}
	return checks, blocked
}

// rollbackImpact explains a rollback decision: whether dependent services
// remain compatible afterwards, and which already-finished batches cannot
// be undone directly and therefore stay in effect.
func rollbackImpact(tx *store.Tx, rel *store.Release) []string {
	var out []string
	for _, b := range releaseBatches(tx, rel.ID) {
		if b.Phase == "succeeded" {
			out = append(out, fmt.Sprintf("第 %d 批已生效（%d 台实例运行在 %s），属于已完成批次，不能直接撤销，将保持现状",
				b.Index+1, len(b.Members), b.TargetVersion))
		}
	}
	if len(out) == 0 {
		out = append(out, "没有已完成的批次，当前批次回退后不遗留已生效变更")
	}
	for _, rule := range tx.CompatRules() {
		if rule.DependencyID != rel.ServiceID {
			continue
		}
		depName := serviceName(tx, rule.DependentID)
		switch rule.Mode {
		case "dependent_first":
			vers := serviceVersions(tx, rule.DependentID)
			latest := ""
			if len(vers) > 0 {
				latest = vers[len(vers)-1].ID
			}
			ahead := 0
			insts := serviceInstances(tx, rule.DependentID)
			for _, inst := range insts {
				if inst.CurrentVersion == latest {
					ahead++
				}
			}
			if ahead == len(insts) && latest != "" {
				out = append(out, fmt.Sprintf("依赖方 %s 已在新版本 %s：回退后其仍兼容旧版依赖，无需联动回退", depName, latest))
			} else {
				out = append(out, fmt.Sprintf("依赖方 %s 仍在旧版：回退后双方版本关系保持兼容", depName))
			}
		case "stay_old":
			out = append(out, fmt.Sprintf("依赖方 %s 按要求保持旧版：回退后仍兼容", depName))
		case "pause":
			if r2 := advancingReleaseOf(tx, rule.DependentID); r2 != nil {
				out = append(out, fmt.Sprintf("依赖方 %s 有正在推进的发布：本服务回退到旧版后其仍可继续，兼容性不受影响", depName))
			} else {
				out = append(out, fmt.Sprintf("依赖方 %s 无推进中的发布：回退不影响其兼容性", depName))
			}
		}
	}
	return out
}

// recordRollbackImpact persists the impact explanation on the release and
// logs it, so a reopen shows exactly why the rollback is safe (or not).
func recordRollbackImpact(tx *store.Tx, rel *store.Release, now time.Time) {
	if len(rel.RollbackImpact) > 0 {
		return
	}
	rel.RollbackImpact = rollbackImpact(tx, rel)
	for _, line := range rel.RollbackImpact {
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "回退影响："+line)
	}
}

// compatEqual compares two verdict lists so the gate only persists (and
// logs) when the evidence actually changed.
func compatEqual(a, b []store.CompatCheck) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
