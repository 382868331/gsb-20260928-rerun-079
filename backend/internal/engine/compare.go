package engine

import (
	"fmt"
	"time"

	"releasectl/internal/store"
)

// Plan comparison: evaluate alternative release arrangements (for example
// "update service A first", "update service B first", "spread batches
// across zones") against the current persisted state, and explain exactly
// which dependency or capacity condition blocks each arrangement. The
// simulation is read-only: nothing here is persisted and no real release
// is created.

type PlanStep struct {
	ServiceID     string `json:"serviceId"`
	TargetVersion string `json:"targetVersion"`
	Strategy      string `json:"strategy"`
	BatchSize     int    `json:"batchSize"`
}

type CompareScenario struct {
	Name  string     `json:"name"`
	Steps []PlanStep `json:"steps"`
}

type CompareRequest struct {
	Scenarios []CompareScenario `json:"scenarios"`
}

type StepExplanation struct {
	ServiceID     string   `json:"serviceId"`
	ServiceName   string   `json:"serviceName"`
	TargetVersion string   `json:"targetVersion"`
	Strategy      string   `json:"strategy"`
	Feasible      bool     `json:"feasible"`
	Skipped       bool     `json:"skipped"`
	Blockers      []string `json:"blockers"`
	Notes         []string `json:"notes"`
}

type ScenarioExplanation struct {
	Name        string            `json:"name"`
	Feasible    bool              `json:"feasible"`
	BlockedStep int               `json:"blockedStep"` // -1 when feasible
	Summary     string            `json:"summary"`
	Steps       []StepExplanation `json:"steps"`
}

// ComparePlans simulates each scenario step by step. A step is feasible
// when its compatibility findings carry no blocking/pausing rule and every
// planned batch fits the capacity floors (current commitments of real
// in-flight releases are honoured for every step, conservatively). When a
// step is blocked the scenario stops there and later steps are marked
// skipped; the summary names the first blocking condition.
func (s *Service) ComparePlans(req CompareRequest, now time.Time) []ScenarioExplanation {
	var out []ScenarioExplanation
	s.st.View(func(tx *store.Tx) {
		for _, sc := range req.Scenarios {
			out = append(out, explainScenario(tx, sc))
		}
	})
	return out
}

func explainScenario(tx *store.Tx, sc CompareScenario) ScenarioExplanation {
	res := ScenarioExplanation{Name: sc.Name, Feasible: true, BlockedStep: -1, Steps: []StepExplanation{}}
	// Simulated per-service versions start at the real current state
	// ("" marks a service whose instances are mid-migration).
	sim := map[string]string{}
	for _, svc := range tx.Services() {
		insts := serviceInstances(tx, svc.ID)
		if len(insts) == 0 {
			continue
		}
		v := insts[0].CurrentVersion
		for _, inst := range insts[1:] {
			if inst.CurrentVersion != v {
				v = ""
			}
		}
		sim[svc.ID] = v
	}
	occ := occupancyMap(tx)
	stats := zoneStats(tx, occ)

	for i, step := range sc.Steps {
		exp := StepExplanation{
			ServiceID: step.ServiceID, ServiceName: serviceName(tx, step.ServiceID),
			TargetVersion: step.TargetVersion, Strategy: step.Strategy,
			Feasible: true, Blockers: []string{}, Notes: []string{},
		}
		if res.BlockedStep >= 0 {
			exp.Skipped = true
			exp.Feasible = false
			exp.Notes = append(exp.Notes, "前序步骤受阻，未评估")
			res.Steps = append(res.Steps, exp)
			continue
		}
		if findService(tx, step.ServiceID) == nil {
			exp.Feasible = false
			exp.Blockers = append(exp.Blockers, "服务不存在："+step.ServiceID)
		} else if len(serviceInstances(tx, step.ServiceID)) == 0 {
			exp.Feasible = false
			exp.Blockers = append(exp.Blockers, "服务暂无虚拟实例")
		} else {
			evaluateStep(tx, sim, occ, stats, step, &exp)
		}
		if !exp.Feasible {
			res.Feasible = false
			res.BlockedStep = i
			res.Summary = fmt.Sprintf("受阻于第 %d 步（%s → %s）：%s",
				i+1, exp.ServiceName, step.TargetVersion, joinComma(exp.Blockers))
		} else {
			sim[step.ServiceID] = step.TargetVersion
		}
		res.Steps = append(res.Steps, exp)
	}
	if res.Feasible {
		res.Summary = fmt.Sprintf("可行：%d 个步骤的依赖与容量条件均满足", len(sc.Steps))
	}
	return res
}

func evaluateStep(tx *store.Tx, sim map[string]string, occ map[string]holder, stats map[zoneServiceKey]*zoneStat, step PlanStep, exp *StepExplanation) {
	// 1. Compatibility dependencies against the simulated fleet.
	for _, f := range evaluateCompatWith(tx, step.ServiceID, step.TargetVersion, sim) {
		switch {
		case !f.Compatible && f.Blocking:
			exp.Blockers = append(exp.Blockers, "依赖约束："+f.Detail)
		case !f.Compatible:
			exp.Blockers = append(exp.Blockers, "依赖审查："+f.Detail)
		default:
			exp.Notes = append(exp.Notes, f.Detail)
		}
	}
	// 2. Capacity: chunk the batches the way the real release would and
	// check every batch against the floors with current commitments.
	insts := serviceInstances(tx, step.ServiceID)
	ordered := orderForStrategy(tx, insts, step.Strategy)
	size := step.BatchSize
	if size < 1 {
		size = 1
	}
	groups := planBatches(ordered, step.Strategy, size)
	for gi, group := range groups {
		demand := map[string]int{}
		for _, instID := range group {
			if inst := findInstance(tx, instID); inst != nil {
				demand[inst.ZoneID]++
			}
		}
		for _, p := range tx.Policies() {
			if p.ServiceID != step.ServiceID {
				continue
			}
			st := statFor(stats, p.ServiceID, p.ZoneID)
			after := st.available - demand[p.ZoneID]
			line := fmt.Sprintf("第 %d 批：%s @ %s 占用 %d 台后可用 %d（下限 %d）",
				gi+1, serviceName(tx, p.ServiceID), zoneName(tx, p.ZoneID), demand[p.ZoneID], after, p.MinAvailable)
			if after < p.MinAvailable {
				exp.Blockers = append(exp.Blockers, "容量条件："+line+" 跌破下限"+holdersText(tx, occ, []string{p.ServiceID}, p.ZoneID))
			} else {
				exp.Notes = append(exp.Notes, line)
			}
		}
		for _, pl := range tx.Pools() {
			if !containsString(pl.ServiceIDs, step.ServiceID) {
				continue
			}
			avail := 0
			for _, sid := range pl.ServiceIDs {
				avail += statFor(stats, sid, pl.ZoneID).available
			}
			after := avail - demand[pl.ZoneID]
			line := fmt.Sprintf("第 %d 批：共享池 %s @ %s 占用 %d 台后可用 %d（下限 %d）",
				gi+1, pl.Name, zoneName(tx, pl.ZoneID), demand[pl.ZoneID], after, pl.MinAvailable)
			if after < pl.MinAvailable {
				exp.Blockers = append(exp.Blockers, "容量条件："+line+" 跌破下限"+holdersText(tx, occ, pl.ServiceIDs, pl.ZoneID))
			} else {
				exp.Notes = append(exp.Notes, line)
			}
		}
	}
	if len(exp.Blockers) > 0 {
		exp.Feasible = false
	}
}
