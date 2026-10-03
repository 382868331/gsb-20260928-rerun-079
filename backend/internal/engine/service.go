package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"releasectl/internal/store"
)

type Service struct {
	st *store.Store
	e  *Engine
}

func NewService(st *store.Store, c Config) *Service {
	s := &Service{st: st, e: New(st, c)}
	st.View(func(tx *store.Tx) {
		n := int64(len(tx.Ops()) + len(tx.Batches()) + len(tx.Releases()))
		for n >= atomicLoad() {
			atomicBump(n)
			break
		}
	})
	return s
}

func (s *Service) Engine() *Engine { return s.e }

// Seed creates the initial local drill environment: two versions and six
// virtual instances on the known-good baseline.
func (s *Service) Seed(now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		if len(tx.Versions()) > 0 || len(tx.Instances()) > 0 {
			return nil
		}
		v1 := store.Version{ID: "v1.0.0", Name: "订单服务 1.0.0（基线）", ArtifactRef: "local://virtual/order-service:1.0.0", CreatedAt: nowStr(now)}
		v2 := store.Version{ID: "v2.0.0", Name: "订单服务 2.0.0（演练目标）", ArtifactRef: "local://virtual/order-service:2.0.0", CreatedAt: nowStr(now)}
		tx.SetVersions([]store.Version{v1, v2})
		tx.SetServices([]store.Service{
			{ID: "svc-order", Name: "订单服务"},
			{ID: "svc-pay", Name: "支付服务"},
			{ID: "svc-inventory", Name: "库存服务"},
		})
		tx.SetRules([]store.CompatRule{
			{ID: "rule-pay-order-v1", ProviderID: "svc-pay", DependentID: "svc-order",
				DependentVersion: "v1.0.0", MinProviderVersion: "v1.0.0", MaxProviderVersion: "v1.9.9",
				Mode: "update_first", Note: "订单服务 v1.0.0 仅兼容支付服务 v1.x"},
			{ID: "rule-pay-order-v2", ProviderID: "svc-pay", DependentID: "svc-order",
				DependentVersion: "v2.0.0", MinProviderVersion: "v1.0.0", MaxProviderVersion: "v2.9.9",
				Mode: "update_first", Note: "订单服务 v2.0.0 起兼容支付服务 v2.x"},
			{ID: "rule-inv-order", ProviderID: "svc-inventory", DependentID: "svc-order",
				MinProviderVersion: "v1.0.0", MaxProviderVersion: "v1.9.9",
				Mode: "pause", Note: "库存服务升级需人工确认订单服务兼容"},
			{ID: "rule-order-pay", ProviderID: "svc-order", DependentID: "svc-pay",
				DependentVersion: "v1.0.0", MinProviderVersion: "v1.0.0", MaxProviderVersion: "v2.9.9",
				Mode: "stay_old", Note: "支付服务保持旧版仍兼容订单服务 v2"},
		})
		tx.SetZones([]store.Zone{
			{ID: "zone-east", Name: "华东区（故障区域）"},
			{ID: "zone-west", Name: "华西区（故障区域）"},
		})
		var insts []store.Instance
		for i := 1; i <= 6; i++ {
			zoneID := "zone-east"
			if i > 3 {
				zoneID = "zone-west"
			}
			insts = append(insts, store.Instance{
				ID: fmt.Sprintf("inst-%d", i), Name: fmt.Sprintf("虚拟实例-%d", i),
				ServiceID:      "svc-order",
				ZoneID:         zoneID,
				CurrentVersion: v1.ID, DesiredVersion: v1.ID, Health: "healthy",
				UpdatedAt: nowStr(now),
			})
		}
		tx.SetInstances(insts)
		tx.AddEvent(nowStr(now), "", "", "", "info", "演练环境就绪：3 个虚拟服务、2 个故障区域、6 个本地虚拟实例、4 条兼容约束，基线版本 v1.0.0；容量下限与共享容量池需在页面中指定")
		return nil
	})
}

type CreateReleaseRequest struct {
	ServiceID     string            `json:"serviceId"`
	TargetVersion string            `json:"targetVersion"`
	BatchSize     int               `json:"batchSize"`
	Strategy      string            `json:"strategy"`
	Failure       store.FailureSpec `json:"failure"`
}

func (s *Service) CreateRelease(req CreateReleaseRequest, now time.Time) (*store.Release, error) {
	if req.BatchSize < 1 {
		req.BatchSize = 2
	}
	if req.Strategy == "" {
		req.Strategy = "rolling"
	}
	var created *store.Release
	err := s.st.Mutate(func(tx *store.Tx) error {
		var target *store.Version
		for i := range tx.Versions() {
			if tx.Versions()[i].ID == req.TargetVersion {
				target = &tx.Versions()[i]
			}
		}
		if target == nil {
			return errors.New("目标版本不存在")
		}
		serviceID := req.ServiceID
		if serviceID == "" && len(tx.Services()) > 0 {
			serviceID = tx.Services()[0].ID
		}
		for i := range tx.Releases() {
			r := tx.Releases()[i]
			if !isReleaseTerminal(r.Status) && r.ServiceID == serviceID {
				return errors.New("该服务已有进行中的发布，同一服务同一时间只运行一个发布")
			}
		}
		var insts []store.Instance
		if serviceID == "" {
			insts = tx.Instances()
		} else {
			if findService(tx, serviceID) == nil {
				return errors.New("目标服务不存在")
			}
			insts = serviceInstances(tx, serviceID)
		}
		if len(insts) == 0 {
			return errors.New("该服务没有虚拟实例")
		}
		baseline := insts[0].DesiredVersion
		for _, inst := range insts {
			if inst.CurrentVersion != insts[0].CurrentVersion {
				return errors.New("服务内实例当前版本不一致，请先统一基线后再发布")
			}
		}
		rel := store.Release{
			ID: newID(tx, "rel"), ServiceID: serviceID, TargetVersion: target.ID, Baseline: baseline,
			Strategy: req.Strategy, BatchSize: req.BatchSize, Status: "pending",
			Failure: req.Failure, CreatedAt: nowStr(now),
		}
		ordered := orderForStrategy(tx, insts, rel.Strategy)
		groups := planBatches(ordered, rel.Strategy, req.BatchSize)
		// Capacity pre-check: refuse a plan whose batches could never fit
		// the agreed floors, even with zero concurrent occupancy. The
		// error names the exact zone/pool condition that blocks the plan.
		if err := validatePlanCapacity(tx, serviceID, groups); err != nil {
			return err
		}
		var batches []store.Batch
		for idx, group := range groups {
			b := store.Batch{ID: newID(tx, "batch"), ReleaseID: rel.ID, Index: idx, Phase: "pending", TargetVersion: target.ID}
			for _, instID := range group {
				from := baseline
				if inst := findInstance(tx, instID); inst != nil {
					from = inst.CurrentVersion
				}
				b.Members = append(b.Members, store.MemberState{
					InstanceID: instID, FromVersion: from, ToVersion: target.ID,
					UpdateState: "pending", HealthState: "pending",
				})
			}
			batches = append(batches, b)
			rel.BatchIDs = append(rel.BatchIDs, b.ID)
		}
		tx.SetReleases(append(tx.Releases(), rel))
		tx.SetBatches(append(tx.Batches(), batches...))
		tx.AddEvent(nowStr(now), rel.ID, "", "", "info",
			fmt.Sprintf("创建发布 %s：服务 %s，策略 %s，%d 个批次，每批 %d 台，目标版本 %s",
				rel.ID, serviceName(tx, serviceID), rel.Strategy, len(batches), rel.BatchSize, target.ID))
		rc := rel
		created = &rc
		return nil
	})
	return created, err
}

func (s *Service) CancelRelease(releaseID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		rel := findRelease(tx, releaseID)
		if rel == nil {
			return errors.New("发布不存在")
		}
		if isReleaseTerminal(rel.Status) {
			return errors.New("发布已结束，无需取消")
		}
		if rel.CancelRequested {
			return nil
		}
		rel.CancelRequested = true
		rel.ReviewAction = "cancel"
		rel.ReviewAt = nowStr(now)
		rel.RollbackNote = rollbackAssessment(tx, rel)
		if rel.Status != "paused" {
			rel.Status = "cancelling"
		}
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "收到取消请求：停止继续扩批；暂停/取消后到达的健康结果只作为证据留档，不再改变终止决定。 "+rel.RollbackNote)
		return nil
	})
}

// rollbackAssessment explains, at the moment the operator chooses to roll
// back, whether dependent services stay compatible and which already
// completed batches can no longer be undone.
func rollbackAssessment(tx *store.Tx, rel *store.Release) string {
	var completed []string
	for _, b := range releaseBatches(tx, rel.ID) {
		if b.Phase == "succeeded" {
			completed = append(completed, batchLabel(b))
		}
	}
	note := ""
	if len(completed) > 0 {
		note = fmt.Sprintf("已完成批次（%s）已生效，不能直接撤销，仅当前进行中批次会回退；",
			joinComma(completed))
	} else {
		note = "尚无已完成批次，当前批次可整体回退；"
	}
	findings := evaluateCompat(tx, rel.ServiceID, rel.Baseline)
	bad := 0
	for _, f := range findings {
		if !f.Compatible {
			bad++
		}
	}
	if bad == 0 {
		note += "回退到基线 " + rel.Baseline + " 后，所有依赖者保持兼容。"
	} else {
		note += fmt.Sprintf("注意：回退到基线 %s 后仍有 %d 条依赖约束不满足，请检查依赖者版本。", rel.Baseline, bad)
	}
	return note
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "、"
		}
		out += s
	}
	return out
}

// EnsureEnvironment creates the default drill environment on demand. The
// normal entry works on a completely empty data directory: nothing is
// pre-written except what this call seeds, and releases are never seeded.
func (s *Service) EnsureEnvironment(now time.Time) error {
	return s.Seed(now)
}

// PauseForReview freezes an in-progress release so a human can read the
// health evidence before choosing to continue, shrink or roll back. Already
// scheduled health ops keep resolving (the answers are the evidence), but no
// new batch expands and no automatic rollback starts while paused.
func (s *Service) PauseForReview(releaseID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		rel := findRelease(tx, releaseID)
		if rel == nil {
			return errors.New("发布不存在")
		}
		if isReleaseTerminal(rel.Status) {
			return errors.New("发布已结束，无需暂停")
		}
		if rel.CancelRequested {
			return errors.New("发布取消已确定，不能再暂停")
		}
		if rel.PauseRequested || rel.Status == "paused" {
			return nil
		}
		rel.PauseRequested = true
		rel.Status = "paused"
		rel.Message = "人工审查暂停：查看健康证据后选择继续、缩小批次或回退"
		for _, b := range releaseBatches(tx, rel.ID) {
			if b.Phase == "updating" || b.Phase == "observing" {
				b.Phase = "paused"
				tx.AddEvent(nowStr(now), rel.ID, b.ID, "", "warn", "人工暂停发布：等待查看健康证据，已排队的健康结果仍会留档")
			}
		}
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "发布进入人工审查暂停，停止扩批与自动决策")
		return nil
	})
}

// ReviewContinue lifts the manual pause: evidence accepted, proceed with the
// original plan. Failure-driven pause still rolls back afterwards.
func (s *Service) ReviewContinue(releaseID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		rel := findRelease(tx, releaseID)
		if rel == nil {
			return errors.New("发布不存在")
		}
		if rel.Status != "paused" || isReleaseTerminal(rel.Status) {
			return errors.New("仅暂停审查中的发布可以继续")
		}
		if rel.CancelRequested {
			return errors.New("取消决定已确定")
		}
		if rel.ReviewAction != "" {
			return errors.New("审查决定已做出：" + rel.ReviewAction)
		}
		if err := rejectOnFailureEvidence(tx, rel, "存在失败/不健康实例，只能选择回退或结束演练"); err != nil {
			return err
		}
		if rel.CompatBlocked {
			findings := evaluateCompat(tx, rel.ServiceID, rel.TargetVersion)
			if hasBlockingFinding(findings) {
				return errors.New("依赖者尚未先更新，兼容约束仍不满足，无法继续")
			}
			rel.CompatBlocked = false
		}
		rel.PauseRequested = false
		rel.ReviewAction = "continue"
		rel.ReviewAt = nowStr(now)
		rel.Status = "in_progress"
		rel.Message = "审查通过，按原批次继续发布"
		tx.AddEvent(nowStr(now), rel.ID, "", "", "info", "人工审查后选择继续：按原批次推进")
		return nil
	})
}

// ReviewShrink lifts the pause and rebuilds every not-yet-started batch so
// later batches use a smaller size. The currently paused batch keeps its
// already-updated instances and is resumed as-is; it never re-runs.
func (s *Service) ReviewShrink(releaseID string, size int, now time.Time) error {
	if size < 1 {
		return errors.New("缩小后的每批实例数至少为 1")
	}
	return s.st.Mutate(func(tx *store.Tx) error {
		rel := findRelease(tx, releaseID)
		if rel == nil {
			return errors.New("发布不存在")
		}
		if rel.Status != "paused" || isReleaseTerminal(rel.Status) {
			return errors.New("仅暂停审查中的发布可以缩小批次")
		}
		if rel.CancelRequested {
			return errors.New("取消决定已确定")
		}
		if rel.ReviewAction != "" {
			return errors.New("审查决定已做出：" + rel.ReviewAction)
		}
		if size >= rel.BatchSize {
			return errors.New("缩小批次的数量必须小于当前每批实例数")
		}
		if err := rejectOnFailureEvidence(tx, rel, "存在失败/不健康实例，无法缩小批次继续，只能回退或结束演练"); err != nil {
			return err
		}
		if rel.CompatBlocked {
			findings := evaluateCompat(tx, rel.ServiceID, rel.TargetVersion)
			if hasBlockingFinding(findings) {
				return errors.New("依赖者尚未先更新，兼容约束仍不满足，无法继续")
			}
			rel.CompatBlocked = false
		}
		if err := rebuildPendingBatches(tx, rel, size, now); err != nil {
			return err
		}
		rel.PauseRequested = false
		rel.ReviewAction = "shrink"
		rel.ReviewShrinkTo = size
		rel.ReviewAt = nowStr(now)
		rel.Status = "in_progress"
		rel.BatchSize = size
		rel.Message = fmt.Sprintf("审查后缩小批次：后续每批 %d 台", size)
		tx.AddEvent(nowStr(now), rel.ID, "", "", "info", fmt.Sprintf("人工审查后选择缩小批次：后续每批 %d 台，已完成批次不重复执行", size))
		return nil
	})
}

// ReviewRollback makes the final rollback decision from a review pause.
// Health results arriving afterwards remain visible as late evidence but can
// never undo the decision.
func (s *Service) ReviewRollback(releaseID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		rel := findRelease(tx, releaseID)
		if rel == nil {
			return errors.New("发布不存在")
		}
		if rel.Status != "paused" || isReleaseTerminal(rel.Status) {
			return errors.New("仅暂停审查中的发布可以回退")
		}
		if rel.CancelRequested {
			return errors.New("取消决定已确定")
		}
		if rel.ReviewAction != "" {
			return errors.New("审查决定已做出：" + rel.ReviewAction)
		}
		rel.ReviewAction = "rollback"
		rel.ReviewAt = nowStr(now)
		rel.ResumeRequested = true
		rel.RollbackNote = rollbackAssessment(tx, rel)
		rel.Message = "审查后决定回退：当前批次返回原版本，后续批次不再扩批"
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "人工审查后选择回退；之后到达的健康结果仅留档，不改变决定。 "+rel.RollbackNote)
		return nil
	})
}

// ResumeRollback continues a paused drill: the paused batch starts its
// rollback exactly where it was frozen. Persisted ops make a page reopen
// resume from the same on-disk state.
func (s *Service) ResumeRollback(releaseID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		rel := findRelease(tx, releaseID)
		if rel == nil {
			return errors.New("发布不存在")
		}
		if isReleaseTerminal(rel.Status) {
			return errors.New("发布已结束")
		}
		if rel.Status != "paused" {
			return errors.New("仅暂停审查中的发布可以继续回退")
		}
		if rel.CancelRequested {
			return errors.New("取消决定已确定")
		}
		if rel.ReviewAction != "" {
			return errors.New("审查决定已做出：" + rel.ReviewAction)
		}
		rel.ResumeRequested = true
		rel.ReviewAction = "rollback"
		rel.ReviewAt = nowStr(now)
		rel.RollbackNote = rollbackAssessment(tx, rel)
		rel.Message = "审查后决定继续回退"
		tx.AddEvent(nowStr(now), rel.ID, "", "", "info", "审查完成，选择继续回退。 "+rel.RollbackNote)
		return nil
	})
}

// FinishDrill ends a paused drill without rolling back, keeping every
// instance on its current (verified on disk) version.
func (s *Service) FinishDrill(releaseID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		rel := findRelease(tx, releaseID)
		if rel == nil {
			return errors.New("发布不存在")
		}
		if isReleaseTerminal(rel.Status) {
			return errors.New("发布已结束")
		}
		if rel.Status != "paused" {
			return errors.New("仅暂停审查中的发布可以结束演练")
		}
		if rel.ReviewAction != "" {
			return errors.New("审查决定已做出：" + rel.ReviewAction)
		}
		rel.FinishRequested = true
		rel.ReviewAction = "finish"
		rel.ReviewAt = nowStr(now)
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "人工审查后选择结束本次演练，保留当前版本分布")
		return nil
	})
}

func (s *Service) CreateVersion(id, name string, now time.Time) (*store.Version, error) {
	if id == "" || name == "" {
		return nil, errors.New("版本 ID 与名称不能为空")
	}
	var out *store.Version
	err := s.st.Mutate(func(tx *store.Tx) error {
		for i := range tx.Versions() {
			if tx.Versions()[i].ID == id {
				return errors.New("版本 ID 已存在")
			}
		}
		v := store.Version{ID: id, Name: name, ArtifactRef: "local://virtual/" + id, CreatedAt: nowStr(now)}
		tx.SetVersions(append(tx.Versions(), v))
		vc := v
		out = &vc
		return nil
	})
	return out, err
}

// AddInstance creates a new virtual instance for a service on the service's
// current baseline (the version its existing instances converge to, else the
// oldest known version). It never touches any real deploy target.
func (s *Service) AddInstance(serviceID string, now time.Time) (*store.Instance, error) {
	var out *store.Instance
	err := s.st.Mutate(func(tx *store.Tx) error {
		svc := findService(tx, serviceID)
		if svc == nil {
			return errors.New("目标服务不存在")
		}
		baseline := ""
		for _, inst := range serviceInstances(tx, serviceID) {
			if baseline == "" || compareVersions(inst.DesiredVersion, baseline) < 0 {
				baseline = inst.DesiredVersion
			}
		}
		if baseline == "" && len(tx.Versions()) > 0 {
			baseline = tx.Versions()[0].ID
		}
		if baseline == "" {
			return errors.New("尚无可用版本，请先创建版本")
		}
		// Place the new instance into the zone where this service currently
		// has the fewest instances, so repeated adds spread across zones.
		zoneID := ""
		if len(tx.Zones()) > 0 {
			count := map[string]int{}
			for _, inst := range tx.Instances() {
				if inst.ServiceID == serviceID {
					count[inst.ZoneID]++
				}
			}
			best := -1
			for _, z := range tx.Zones() {
				if best == -1 || count[z.ID] < best {
					best = count[z.ID]
					zoneID = z.ID
				}
			}
		}
		id := ""
		for {
			candidate := newID(tx, "inst")
			if findInstance(tx, candidate) == nil {
				id = candidate
				break
			}
		}
		inst := store.Instance{
			ID: id, Name: svc.Name + "-" + id, ServiceID: serviceID,
			ZoneID:         zoneID,
			CurrentVersion: baseline, DesiredVersion: baseline,
			Health: "healthy", UpdatedAt: nowStr(now),
		}
		tx.SetInstances(append(tx.Instances(), inst))
		tx.AddEvent(nowStr(now), "", "", inst.ID, "info", "新增虚拟实例 "+inst.ID+"（"+svc.Name+" @ "+zoneName(tx, zoneID)+"，基线 "+baseline+"）")
		ic := inst
		out = &ic
		return nil
	})
	return out, err
}

// AddRule registers a compatibility constraint between two virtual services.
func (s *Service) AddRule(rule store.CompatRule, now time.Time) (*store.CompatRule, error) {
	if rule.ProviderID == "" || rule.DependentID == "" {
		return nil, errors.New("约束必须指定服务与依赖者")
	}
	if rule.ProviderID == rule.DependentID {
		return nil, errors.New("服务不能依赖自身")
	}
	switch rule.Mode {
	case "update_first", "stay_old", "pause":
	default:
		return nil, errors.New("未知的约束处理方式：" + rule.Mode)
	}
	var out *store.CompatRule
	err := s.st.Mutate(func(tx *store.Tx) error {
		if findService(tx, rule.ProviderID) == nil || findService(tx, rule.DependentID) == nil {
			return errors.New("约束涉及的服务不存在")
		}
		rule.ID = newID(tx, "rule")
		tx.SetRules(append(tx.Rules(), rule))
		tx.AddEvent(nowStr(now), "", "", "", "info",
			fmt.Sprintf("新增兼容约束 %s：%s 更新影响依赖者 %s（%s）",
				rule.ID, serviceName(tx, rule.ProviderID), serviceName(tx, rule.DependentID), rule.Mode))
		rc := rule
		out = &rc
		return nil
	})
	return out, err
}

// DependencyStatuses computes, for every in-flight release, the compatibility
// constraints that are not yet satisfied (unfinished dependencies).
func (s *Service) DependencyStatuses() []store.DependencyStatus {
	var out []store.DependencyStatus
	s.st.View(func(tx *store.Tx) {
		for _, rel := range tx.Releases() {
			if isReleaseTerminal(rel.Status) {
				continue
			}
			st := store.DependencyStatus{ReleaseID: rel.ID, ServiceID: rel.ServiceID}
			for _, f := range evaluateCompat(tx, rel.ServiceID, rel.TargetVersion) {
				if f.Compatible {
					continue
				}
				st.Pending = append(st.Pending, f.Detail)
				if f.Blocking {
					st.Blocked = true
				}
			}
			if rel.CapacityBlocked && rel.CapacityNote != "" {
				st.Pending = append(st.Pending, "容量："+rel.CapacityNote)
				st.Blocked = true
			}
			if len(st.Pending) > 0 {
				out = append(out, st)
			}
		}
		// Mutual-wait detection: two in-flight releases each blocked on the
		// other side (a dependency on the other's service, or capacity the
		// other still holds). Mark both so the drill operator sees that
		// only an explicit decision (cancel one side, or clear the
		// condition) can move the drill on.
		for i := 0; i < len(out); i++ {
			for j := i + 1; j < len(out); j++ {
				a, b := out[i], out[j]
				if waitsOn(tx, a, b.ServiceID, b.ReleaseID) && waitsOn(tx, b, a.ServiceID, a.ReleaseID) {
					out[i].Mutual = true
					out[j].Mutual = true
				}
			}
		}
	})
	return out
}

// waitsOn reports whether release a is currently blocked by something
// belonging to the other side: an unmet update_first rule whose dependent
// is otherServiceID, or a capacity note that names the other release.
func waitsOn(tx *store.Tx, a store.DependencyStatus, otherServiceID, otherReleaseID string) bool {
	if rel := findRelease(tx, a.ReleaseID); rel != nil && rel.CapacityBlocked && otherReleaseID != "" &&
		len(rel.CapacityNote) > 0 && strings.Contains(rel.CapacityNote, otherReleaseID) {
		return true
	}
	for _, f := range evaluateCompat(tx, a.ServiceID, targetVersionOf(tx, a.ReleaseID)) {
		if !f.Compatible && f.Blocking && f.DependentID == otherServiceID {
			return true
		}
	}
	return false
}

func targetVersionOf(tx *store.Tx, releaseID string) string {
	if rel := findRelease(tx, releaseID); rel != nil {
		return rel.TargetVersion
	}
	return ""
}

// AddPolicy registers the minimum available capacity one service must keep
// in one zone at all times.
func (s *Service) AddPolicy(p store.CapacityPolicy, now time.Time) (*store.CapacityPolicy, error) {
	if p.ServiceID == "" || p.ZoneID == "" {
		return nil, errors.New("容量下限必须指定服务与故障区域")
	}
	if p.MinAvailable < 0 {
		return nil, errors.New("最低可用实例数不能为负")
	}
	var out *store.CapacityPolicy
	err := s.st.Mutate(func(tx *store.Tx) error {
		if findService(tx, p.ServiceID) == nil {
			return errors.New("服务不存在")
		}
		if zoneByID(tx, p.ZoneID) == nil {
			return errors.New("故障区域不存在")
		}
		for _, existing := range tx.Policies() {
			if existing.ServiceID == p.ServiceID && existing.ZoneID == p.ZoneID {
				return errors.New("该服务在此区域已有容量下限，请先调整既有约定")
			}
		}
		p.ID = newID(tx, "policy")
		tx.SetPolicies(append(tx.Policies(), p))
		tx.AddEvent(nowStr(now), "", "", "", "info",
			fmt.Sprintf("新增容量下限 %s：%s @ %s 至少保留 %d 台可用实例",
				p.ID, serviceName(tx, p.ServiceID), zoneName(tx, p.ZoneID), p.MinAvailable))
		pc := p
		out = &pc
		return nil
	})
	return out, err
}

// AddPool registers a shared capacity floor several services depend on.
func (s *Service) AddPool(pl store.SharedPool, now time.Time) (*store.SharedPool, error) {
	if pl.ZoneID == "" {
		return nil, errors.New("共享容量池必须指定故障区域")
	}
	if len(pl.ServiceIDs) < 2 {
		return nil, errors.New("共享容量池至少包含两个服务")
	}
	if pl.MinAvailable < 0 {
		return nil, errors.New("最低可用实例数不能为负")
	}
	if pl.Name == "" {
		pl.Name = "共享容量池"
	}
	var out *store.SharedPool
	err := s.st.Mutate(func(tx *store.Tx) error {
		if zoneByID(tx, pl.ZoneID) == nil {
			return errors.New("故障区域不存在")
		}
		seen := map[string]bool{}
		for _, sid := range pl.ServiceIDs {
			if findService(tx, sid) == nil {
				return errors.New("共享容量池涉及的服务不存在：" + sid)
			}
			if seen[sid] {
				return errors.New("共享容量池包含重复服务：" + sid)
			}
			seen[sid] = true
		}
		pl.ID = newID(tx, "pool")
		tx.SetPools(append(tx.Pools(), pl))
		names := make([]string, 0, len(pl.ServiceIDs))
		for _, sid := range pl.ServiceIDs {
			names = append(names, serviceName(tx, sid))
		}
		tx.AddEvent(nowStr(now), "", "", "", "info",
			fmt.Sprintf("新增共享容量池 %s：%s @ %s 合计至少保留 %d 台可用实例",
				pl.Name, joinComma(names), zoneName(tx, pl.ZoneID), pl.MinAvailable))
		pc := pl
		out = &pc
		return nil
	})
	return out, err
}

// FailZone injects a zone failure: every virtual instance in the zone
// becomes unavailable. In-flight health observations there resolve
// unhealthy, and capacity floors immediately see the lost capacity.
func (s *Service) FailZone(zoneID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		z := zoneByID(tx, zoneID)
		if z == nil {
			return errors.New("故障区域不存在")
		}
		if z.Failed {
			return nil
		}
		zones := append([]store.Zone(nil), tx.Zones()...)
		for i := range zones {
			if zones[i].ID == zoneID {
				zones[i].Failed = true
				zones[i].FailedAt = nowStr(now)
			}
		}
		tx.SetZones(zones)
		affected := 0
		insts := append([]store.Instance(nil), tx.Instances()...)
		for i := range insts {
			if insts[i].ZoneID == zoneID {
				insts[i].Health = "unhealthy"
				insts[i].UpdatedAt = nowStr(now)
				affected++
			}
		}
		tx.SetInstances(insts)
		tx.AddEvent(nowStr(now), "", "", "", "error",
			fmt.Sprintf("故障区域失效：%s，%d 台虚拟实例不可用；进行中的健康观察将判定不健康，容量下限按失效后余量计算", zoneName(tx, zoneID), affected))
		return nil
	})
}

// RecoverZone ends a zone failure. Instances not currently occupied by an
// in-flight batch become healthy again; occupied ones keep following their
// own update/rollback ops.
func (s *Service) RecoverZone(zoneID string, now time.Time) error {
	return s.st.Mutate(func(tx *store.Tx) error {
		z := zoneByID(tx, zoneID)
		if z == nil {
			return errors.New("故障区域不存在")
		}
		if !z.Failed {
			return nil
		}
		zones := append([]store.Zone(nil), tx.Zones()...)
		for i := range zones {
			if zones[i].ID == zoneID {
				zones[i].Failed = false
				zones[i].FailedAt = ""
			}
		}
		tx.SetZones(zones)
		occ := occupancyMap(tx)
		insts := append([]store.Instance(nil), tx.Instances()...)
		for i := range insts {
			if insts[i].ZoneID != zoneID {
				continue
			}
			if _, held := occ[insts[i].ID]; held {
				continue
			}
			insts[i].Health = "healthy"
			insts[i].UpdatedAt = nowStr(now)
		}
		tx.SetInstances(insts)
		tx.AddEvent(nowStr(now), "", "", "", "info", "故障区域恢复："+zoneName(tx, zoneID)+"，未被占用的实例恢复可用")
		return nil
	})
}
