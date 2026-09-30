package engine

import (
	"fmt"
	"sort"
	"time"

	"releasectl/internal/store"
)

type Config struct {
	UpdateDelay   time.Duration
	HealthDelay   time.Duration
	RollbackDelay time.Duration
}

func DefaultConfig() Config {
	return Config{UpdateDelay: 2 * time.Second, HealthDelay: 3 * time.Second, RollbackDelay: 2 * time.Second}
}

type Engine struct {
	st *store.Store
	c  Config
}

func New(st *store.Store, c Config) *Engine { return &Engine{st: st, c: c} }

// Tick resolves every operation due at now and then reconciles batch and
// release state. Persistence lives inside the store transaction, so a crash
// between ticks resumes cleanly (including a rollback that was interrupted).
func (e *Engine) Tick(now time.Time) error {
	return e.st.Mutate(func(tx *store.Tx) error {
		e.resolveDueOps(tx, now)
		for {
			did, err := e.reconcile(tx, now)
			if err != nil {
				return err
			}
			if !did {
				break
			}
		}
		return nil
	})
}

func (e *Engine) resolveDueOps(tx *store.Tx, now time.Time) {
	due := map[string]store.Op{}
	remaining := tx.Ops()[:0]
	for _, op := range tx.Ops() {
		t, err := time.Parse(time.RFC3339Nano, op.DueAt)
		if err == nil && !t.After(now) {
			due[op.ID] = op
		} else {
			remaining = append(remaining, op)
		}
	}
	if len(due) == 0 {
		return
	}
	tx.SetOps(remaining)

	ids := make([]string, 0, len(due))
	for id := range due {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		op := due[id]
		switch op.Kind {
		case "update":
			e.resolveUpdate(tx, op, now)
		case "health":
			e.resolveHealth(tx, op, now)
		case "rollback":
			e.resolveRollback(tx, op, now)
		}
	}
}

func (e *Engine) resolveUpdate(tx *store.Tx, op store.Op, now time.Time) {
	b, mi := findMember(tx, op.BatchID, op.InstanceID)
	if b == nil || mi < 0 {
		return
	}
	m := &b.Members[mi]
	if m.UpdateState != "in_progress" {
		return // superseded
	}
	rel := findRelease(tx, op.ReleaseID)
	fail := rel != nil && updateShouldFail(*rel, *b, *m)
	inst := findInstance(tx, op.InstanceID)
	if fail {
		m.UpdateState = "failed"
		m.Message = "虚拟更新失败，实例仍停留在原版本"
		if inst != nil {
			inst.Health = "unhealthy"
			inst.UpdatedAt = nowStr(now)
		}
		tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "error", "实例更新到 "+op.ToVersion+" 失败，触发批次回退")
		return
	}
	m.UpdateState = "succeeded"
	m.Message = "已更新到新版本，等待健康观察"
	m.HealthState = "in_progress"
	if inst != nil {
		inst.CurrentVersion = op.ToVersion
		inst.Health = "unknown"
		inst.UpdatedAt = nowStr(now)
	}
	health := store.Op{
		ID: newID(tx, "op"), Kind: "health", ReleaseID: op.ReleaseID, BatchID: op.BatchID,
		InstanceID: op.InstanceID, ToVersion: op.ToVersion, ResolveVersion: op.ToVersion,
		DueAt: now.Add(e.c.HealthDelay).Format(time.RFC3339Nano),
	}
	tx.SetOps(append(tx.Ops(), health))
	if b.Phase == "updating" {
		b.Phase = "observing"
	}
	tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "info", "实例已更新到 "+op.ToVersion+"，开始健康观察")
}

func (e *Engine) resolveHealth(tx *store.Tx, op store.Op, now time.Time) {
	b, mi := findMember(tx, op.BatchID, op.InstanceID)
	if b == nil || mi < 0 {
		return
	}
	m := &b.Members[mi]
	key := op.ReleaseID + ":" + op.BatchID + ":" + op.ResolveVersion
	terminal := m.HealthState == "healthy" || m.HealthState == "unhealthy" || m.HealthState == "stale"
	inst := findInstance(tx, op.InstanceID)
	// Late-result guard: the answer only counts for the exact
	// release+batch+version that asked. Once the instance moved away (rollback
	// or a later release to a newer version) the result is recorded stale and
	// can never be attributed to the next version.
	movedOn := inst != nil && inst.CurrentVersion != op.ResolveVersion
	if terminal {
		// A terminal decision (healthy/unhealthy/stale, or an already
		// rolled-back/cancelled member) is final: the late result stays
		// queryable in the event log but must not change any state.
		tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "info",
			"健康结果在终止决定之后到达（版本 "+op.ResolveVersion+"），仅留档可查，不改变已确定的状态")
		return
	}
	if m.LastResolvedKey == key || m.UpdateState == "rolled_back" || m.UpdateState == "rollback_in_progress" || b.Phase == "rolling_back" || m.ToVersion != op.ResolveVersion || movedOn {
		m.HealthState = "stale"
		m.Message = "健康结果迟到：实例已不在该版本，结果作废，不计入后续版本"
		tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "warn", "迟到的健康结果被忽略（期望版本 "+op.ResolveVersion+"），未归入任何后续版本")
		return
	}
	healthy := true
	if rel := findRelease(tx, op.ReleaseID); rel != nil {
		healthy = !healthShouldFail(*rel, *b, *m)
	}
	m.LastResolvedKey = key
	if healthy {
		m.HealthState = "healthy"
		m.Message = "新版本健康检查通过"
		if inst != nil {
			inst.Health = "healthy"
			inst.UpdatedAt = nowStr(now)
		}
		tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "info", "新版本 "+op.ResolveVersion+" 健康检查通过")
	} else {
		m.HealthState = "unhealthy"
		m.Message = "健康检查失败，等待回退"
		if inst != nil {
			inst.Health = "unhealthy"
			inst.UpdatedAt = nowStr(now)
		}
		tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "error", "新版本 "+op.ResolveVersion+" 健康检查失败，触发批次回退")
	}
}

func (e *Engine) resolveRollback(tx *store.Tx, op store.Op, now time.Time) {
	b, mi := findMember(tx, op.BatchID, op.InstanceID)
	if b == nil || mi < 0 {
		return
	}
	m := &b.Members[mi]
	if m.UpdateState != "rollback_in_progress" {
		return
	}
	rel := findRelease(tx, op.ReleaseID)
	// A rollback op can itself be interrupted N times. Retries are persisted
	// ops, so killing the process mid-rollback resumes here on restart.
	if rel != nil && op.Attempt < rel.Failure.FailFirstNRollback {
		op.Attempt++
		op.DueAt = now.Add(e.c.RollbackDelay).Format(time.RFC3339Nano)
		tx.SetOps(append(tx.Ops(), op))
		tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "error", fmt.Sprintf("回退中断（第 %d 次），稍后自动重试", op.Attempt))
		return
	}
	inst := findInstance(tx, op.InstanceID)
	if inst != nil {
		inst.CurrentVersion = op.FromVersion
		inst.Health = "healthy"
		inst.UpdatedAt = nowStr(now)
	}
	m.UpdateState = "rolled_back"
	if m.HealthState == "pending" || m.HealthState == "in_progress" {
		m.HealthState = "stale"
	}
	m.Message = "已回退到原版本 " + op.FromVersion + "，归宿明确"
	tx.AddEvent(nowStr(now), op.ReleaseID, op.BatchID, op.InstanceID, "warn", "实例已回退到原版本 "+op.FromVersion)
}
