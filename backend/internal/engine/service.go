package engine

import (
	"errors"
	"fmt"
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
		var insts []store.Instance
		for i := 1; i <= 6; i++ {
			insts = append(insts, store.Instance{
				ID: fmt.Sprintf("inst-%d", i), Name: fmt.Sprintf("虚拟实例-%d", i),
				CurrentVersion: v1.ID, DesiredVersion: v1.ID, Health: "healthy",
				UpdatedAt: nowStr(now),
			})
		}
		tx.SetInstances(insts)
		tx.AddEvent(nowStr(now), "", "", "", "info", "演练环境就绪：6 个本地虚拟实例，基线版本 v1.0.0")
		return nil
	})
}

type CreateReleaseRequest struct {
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
		for i := range tx.Releases() {
			r := tx.Releases()[i]
			if !isReleaseTerminal(r.Status) {
				return errors.New("已有进行中的发布，演练平台同一时间只运行一个发布")
			}
		}
		insts := tx.Instances()
		if len(insts) == 0 {
			return errors.New("没有虚拟实例")
		}
		baseline := insts[0].DesiredVersion
		for _, inst := range insts {
			if inst.CurrentVersion != insts[0].CurrentVersion {
				return errors.New("实例当前版本不一致，请先统一基线后再发布")
			}
		}
		rel := store.Release{
			ID: newID(tx, "rel"), TargetVersion: target.ID, Baseline: baseline,
			Strategy: req.Strategy, BatchSize: req.BatchSize, Status: "pending",
			Failure: req.Failure, CreatedAt: nowStr(now),
		}
		var batches []store.Batch
		idx := 0
		size := req.BatchSize
		if req.Strategy == "all_at_once" {
			size = len(insts)
		}
		for start := 0; start < len(insts); start += size {
			end := start + size
			if end > len(insts) {
				end = len(insts)
			}
			b := store.Batch{ID: newID(tx, "batch"), ReleaseID: rel.ID, Index: idx, Phase: "pending", TargetVersion: target.ID}
			for _, inst := range insts[start:end] {
				from := inst.CurrentVersion
				b.Members = append(b.Members, store.MemberState{
					InstanceID: inst.ID, FromVersion: from, ToVersion: target.ID,
					UpdateState: "pending", HealthState: "pending",
				})
			}
			batches = append(batches, b)
			rel.BatchIDs = append(rel.BatchIDs, b.ID)
			idx++
		}
		tx.SetReleases(append(tx.Releases(), rel))
		tx.SetBatches(append(tx.Batches(), batches...))
		tx.AddEvent(nowStr(now), rel.ID, "", "", "info",
			fmt.Sprintf("创建发布 %s：策略 %s，%d 个批次，每批 %d 台，目标版本 %s",
				rel.ID, rel.Strategy, len(batches), rel.BatchSize, target.ID))
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
		if rel.Status != "paused" {
			rel.Status = "cancelling"
		}
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "收到取消请求：停止继续扩批；暂停/取消后到达的健康结果只作为证据留档，不再改变终止决定")
		return nil
	})
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
		rel.PauseRequested = false
		rel.ReviewAction = "continue"
		rel.ReviewAt = nowStr(now)
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
		if err := rebuildPendingBatches(tx, rel, size, now); err != nil {
			return err
		}
		rel.PauseRequested = false
		rel.ReviewAction = "shrink"
		rel.ReviewShrinkTo = size
		rel.ReviewAt = nowStr(now)
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
		rel.Message = "审查后决定回退：当前批次返回原版本，后续批次不再扩批"
		tx.AddEvent(nowStr(now), rel.ID, "", "", "warn", "人工审查后选择回退；之后到达的健康结果仅留档，不改变决定")
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
		rel.Message = "审查后决定继续回退"
		tx.AddEvent(nowStr(now), rel.ID, "", "", "info", "审查完成，选择继续回退")
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
