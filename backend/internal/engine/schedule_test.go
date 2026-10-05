package engine_test

import (
	"strings"
	"testing"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// maxSharedEver tracks the highest shared capacity each zone ever had, so
// the oversell check tolerates mid-run downsizes (commitments acquired
// under the old, larger limit stay valid).
type capacityWatch struct {
	maxShared map[string]int
}

func newCapacityWatch() *capacityWatch { return &capacityWatch{maxShared: map[string]int{}} }

// check asserts the two drill invariants after every step: no oversell
// (occupied slots never exceed the largest configured shared capacity) and
// no double use / double release (an instance appears in at most one held
// grant; a released grant is empty and carries its evidence).
func (w *capacityWatch) check(t *testing.T, st *store.Store) {
	t.Helper()
	for _, z := range zonesOf(st) {
		if z.SharedCapacity > w.maxShared[z.ID] {
			w.maxShared[z.ID] = z.SharedCapacity
		}
	}
	occupied := map[string]map[string]bool{} // zone -> instance set
	for _, g := range grantsOf(st) {
		if g.State == "held" {
			for _, id := range g.InstanceIDs {
				if occupied[g.ZoneID] == nil {
					occupied[g.ZoneID] = map[string]bool{}
				}
				if occupied[g.ZoneID][id] {
					t.Fatalf("实例 %s 同时出现在两条持有中的容量承诺（重复占用）", id)
				}
				occupied[g.ZoneID][id] = true
			}
			continue
		}
		if len(g.InstanceIDs) != 0 {
			t.Fatalf("已释放承诺仍挂着实例（重复释放风险）: %+v", g)
		}
		if g.ReleasedAt == "" || g.ReleaseWhy == "" {
			t.Fatalf("已释放承诺缺少释放时间与原因: %+v", g)
		}
	}
	for zid, insts := range occupied {
		if max := w.maxShared[zid]; max > 0 && len(insts) > max {
			t.Fatalf("故障区域 %s 占用 %d 台，超过历史最大共享容量 %d 台（超卖）", zid, len(insts), max)
		}
	}
}

func zonesOf(st *store.Store) []store.Zone {
	var out []store.Zone
	st.View(func(tx *store.Tx) { out = append(out, tx.Zones()...) })
	return out
}

func plansOf(st *store.Store) []store.PlanRecord {
	var out []store.PlanRecord
	st.View(func(tx *store.Tx) { out = append(out, tx.Plans()...) })
	return out
}

func eventsOf(st *store.Store) []store.Event {
	var out []store.Event
	st.View(func(tx *store.Tx) { out = append(out, tx.Events()...) })
	return out
}

func batchOf(st *store.Store, releaseID string, index int) *store.Batch {
	for i, b := range batchesOf(st) {
		if b.ReleaseID == releaseID && b.Index == index {
			bb := batchesOf(st)[i]
			return &bb
		}
	}
	return nil
}

func releaseOf(st *store.Store, id string) *store.Release {
	for _, r := range releasesOf(st) {
		if r.ID == id {
			return &r
		}
	}
	return nil
}

// addPayInstances gives the payment service two virtual instances (one per
// zone) and registers a compatible target version for its releases.
func addPayInstances(t *testing.T, svc *engine.Service, now time.Time) {
	t.Helper()
	if _, err := svc.CreateVersion("v1.5.0", "支付服务 1.5.0", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddInstance("svc-pay", now); err != nil {
		t.Fatal(err)
	}
}

// A high-priority release queued behind a low-priority one must not
// preempt the in-flight batch, but its not-yet-executed batches jump the
// queue: the freed slot goes to the high-priority release first, and the
// persisted plan shows the coordinated order.
func TestPriorityQueueJumpWithoutPreemption(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	watch := newCapacityWatch()

	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	addPayInstances(t, svc, now)

	relLow, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1, Priority: 0,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Let the low-priority release's first batch actually start updating.
	runUntil(t, svc, &now, 150*time.Millisecond)
	watch.check(t, snapshotStore(t, dir))

	relHigh, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 1, Priority: 5,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 50*time.Millisecond)
	st := snapshotStore(t, dir)
	watch.check(t, st)

	// The high-priority batch waits for the shared slot and the persisted
	// reason names the holder; the in-flight batch is never preempted.
	b := batchOf(st, relHigh.ID, 0)
	if b == nil || b.CapacityWait == "" {
		t.Fatalf("高优先级批次应等待共享容量: %+v", b)
	}
	if !strings.Contains(b.CapacityWait, "共享容量") || !strings.Contains(b.CapacityWait, relLow.ID) {
		t.Fatalf("等待原因应说明共享容量与占用方 %s: %s", relLow.ID, b.CapacityWait)
	}

	// The coordinated plan produced at creation time orders every
	// not-yet-executed high-priority batch ahead of the low-priority one.
	var plan *store.PlanRecord
	for i, p := range plansOf(st) {
		if strings.Contains(p.Trigger, relHigh.ID) {
			pp := plansOf(st)[i]
			plan = &pp
		}
	}
	if plan == nil || len(plan.Entries) == 0 {
		t.Fatal("创建高优先级发布应产生协调计划")
	}
	if plan.Entries[0].ReleaseID != relHigh.ID {
		t.Fatalf("计划首个批次应属于高优先级发布 %s，实际 %s", relHigh.ID, plan.Entries[0].ReleaseID)
	}
	for _, e := range plan.Entries {
		if e.ReleaseID == relLow.ID {
			continue
		}
		if e.ReleaseID != relHigh.ID {
			t.Fatalf("计划中出现未知发布 %s", e.ReleaseID)
		}
	}

	// No preemption: the low-priority in-flight instance finishes its
	// update normally while the high-priority batch waits.
	runUntil(t, svc, &now, 200*time.Millisecond)
	st = snapshotStore(t, dir)
	for _, inst := range allInstances(st) {
		if inst.ServiceID == "svc-pay" && inst.ZoneID == "zone-a" && inst.CurrentVersion != "v1.5.0" {
			t.Fatalf("进行中的实例被抢占：%s 仍在 %s", inst.ID, inst.CurrentVersion)
		}
	}
	watch.check(t, st)

	// The freed slot is handed to the high-priority release next; only
	// after the old instances were confirmed returned.
	runUntil(t, svc, &now, 5*time.Second)
	st = snapshotStore(t, dir)
	watch.check(t, st)
	var zoneAGrants []store.CapacityGrant
	for _, g := range grantsOf(st) {
		if g.ZoneID == "zone-a" {
			zoneAGrants = append(zoneAGrants, g)
		}
	}
	if len(zoneAGrants) < 2 {
		t.Fatalf("zone-a 应有多条容量承诺，实际 %d 条", len(zoneAGrants))
	}
	if zoneAGrants[0].ReleaseID != relLow.ID || zoneAGrants[1].ReleaseID != relHigh.ID {
		t.Fatalf("容量交接顺序应为 低优先级 -> 高优先级，实际 %s -> %s",
			zoneAGrants[0].ReleaseID, zoneAGrants[1].ReleaseID)
	}
	if got := releaseOf(st, relHigh.ID).Status; got != "succeeded" {
		t.Fatalf("高优先级发布应成功，实际 %s", got)
	}
	if got := releaseOf(st, relLow.ID).Status; got != "succeeded" {
		t.Fatalf("低优先级发布最终也应成功，实际 %s", got)
	}
}

// Aging: a low-priority release waiting for shared capacity eventually
// outranks a stream of higher-priority batches, so it cannot starve.
func TestAgingPreventsStarvation(t *testing.T) {
	dir := t.TempDir()
	st0, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := engine.NewService(st0, engine.Config{
		UpdateDelay: dUpdate, HealthDelay: dHealth, RollbackDelay: dRollback,
		AgingUnit: 50 * time.Millisecond,
	})
	if err := svc.Seed(t0); err != nil {
		t.Fatal(err)
	}
	now := t0
	watch := newCapacityWatch()

	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	addPayInstances(t, svc, now)

	// High-priority release: every batch touches zone-a, batches run
	// back-to-back and would monopolize the slot without aging.
	relHigh, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Priority: 5,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 100*time.Millisecond)
	relLow, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1, Priority: 0,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the low-priority release acquires the shared slot.
	var acquired bool
	for i := 0; i < 400 && !acquired; i++ {
		runUntil(t, svc, &now, 25*time.Millisecond)
		st := snapshotStore(t, dir)
		watch.check(t, st)
		for _, g := range grantsOf(st) {
			if g.ReleaseID == relLow.ID && g.State == "held" {
				acquired = true
			}
		}
	}
	if !acquired {
		t.Fatal("低优先级发布一直未取得容量（饥饿）")
	}
	st := snapshotStore(t, dir)
	if got := releaseOf(st, relHigh.ID).Status; got == "succeeded" {
		t.Fatal("低优先级发布不应等到高优先级全部批次结束后才取得容量")
	}
	pendingLeft := 0
	for _, b := range batchesOf(st) {
		if b.ReleaseID == relHigh.ID && b.Phase == "pending" {
			pendingLeft++
		}
	}
	if pendingLeft == 0 {
		t.Fatal("低优先级发布应在高优先级仍有待执行批次时凭等待时长取得容量")
	}
	foundYield := false
	for _, e := range eventsOf(st) {
		if strings.Contains(e.Message, "让位于更高优先级") {
			foundYield = true
		}
	}
	if !foundYield {
		t.Fatal("应记录低优先级批次曾让位于高优先级发布的事件")
	}

	runUntil(t, svc, &now, 5*time.Second)
	st = snapshotStore(t, dir)
	watch.check(t, st)
	if got := releaseOf(st, relLow.ID).Status; got != "succeeded" {
		t.Fatalf("低优先级发布最终应成功，实际 %s", got)
	}
	if got := releaseOf(st, relHigh.ID).Status; got != "succeeded" {
		t.Fatalf("高优先级发布最终应成功，实际 %s", got)
	}
}

// Zone downsize + failure rollback + cancel interleaved: held commitments
// survive the downsize, the rollback keeps really-occupied slots until
// every instance is verified back, and only the affected later batches are
// re-planned. Oversell and double release are checked after every step.
func TestDownsizeRollbackCancelInterleaved(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	watch := newCapacityWatch()

	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 2, now); err != nil {
		t.Fatal(err)
	}
	addPayInstances(t, svc, now)

	rel1, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Priority: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 100*time.Millisecond)
	rel2, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1, Priority: 0,
		// half_batch fails the health check after the instance really
		// moved to the new version, so its slot stays occupied until the
		// rollback confirms the return.
		Failure: store.FailureSpec{Mode: "half_batch", PauseOnFailure: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Wait until rel2's batch pauses on the injected health failure: the
	// instance really moved to the new version, so its slot must stay
	// occupied while the operator reviews (no planned-return pretending).
	paused := false
	for i := 0; i < 200 && !paused; i++ {
		runUntil(t, svc, &now, 25*time.Millisecond)
		st := snapshotStore(t, dir)
		watch.check(t, st)
		paused = releaseOf(st, rel2.ID).Status == "paused"
	}
	st := snapshotStore(t, dir)
	if !paused {
		t.Fatal("rel2 应因健康失败进入暂停审查")
	}
	heldBy := func(relID string) int {
		n := 0
		for _, g := range grantsOf(snapshotStore(t, dir)) {
			if g.ReleaseID == relID && g.ZoneID == "zone-a" && g.State == "held" {
				n += len(g.InstanceIDs)
			}
		}
		return n
	}
	if n := heldBy(rel2.ID); n != 1 {
		t.Fatalf("失败批次实际占用的容量必须保留（rel2 应持有 1 台，实际 %d）", n)
	}
	rel1HeldBefore := heldBy(rel1.ID)
	if rel1HeldBefore == 0 {
		t.Fatal("rel1 应持有 zone-a 容量承诺")
	}

	// Downsize the zone mid-run: shared capacity 2 -> 1. Commitments
	// already acquired by both releases must stay valid; only the
	// not-yet-executed batches are re-planned.
	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	st = snapshotStore(t, dir)
	watch.check(t, st)
	if n := heldBy(rel2.ID); n != 1 {
		t.Fatalf("降容不得回收已取得的承诺（rel2 持有 %d 台）", n)
	}
	if n := heldBy(rel1.ID); n != rel1HeldBefore {
		t.Fatalf("降容不得影响其他发布已取得的承诺（rel1 持有 %d -> %d）", rel1HeldBefore, n)
	}
	var downPlan *store.PlanRecord
	for i, p := range plansOf(st) {
		if strings.Contains(p.Trigger, "调整区域") {
			pp := plansOf(st)[i]
			downPlan = &pp
		}
	}
	if downPlan == nil {
		t.Fatal("区域降容应触发一次协调重排并记录原因")
	}

	// Resume the rollback: the slot is released only after the instance is
	// verified back on its original version.
	if err := svc.ResumeRollback(rel2.ID, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200 && releaseOf(snapshotStore(t, dir), rel2.ID).Status != "rolled_back"; i++ {
		runUntil(t, svc, &now, 25*time.Millisecond)
		watch.check(t, snapshotStore(t, dir))
	}
	st = snapshotStore(t, dir)
	if got := releaseOf(st, rel2.ID).Status; got != "rolled_back" {
		t.Fatalf("rel2 应完成回退，实际 %s", got)
	}
	if n := heldBy(rel2.ID); n != 0 {
		t.Fatalf("回退完成后 rel2 的容量应全部释放，仍持有 %d 台", n)
	}
	for _, inst := range allInstances(st) {
		if inst.ID == "inst-7" && inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("inst-7 应回到原版本，实际 %s", inst.CurrentVersion)
		}
	}

	// Cancel rel1 mid-run: only its affected later batches are re-planned;
	// the in-flight batch rolls back and every commitment is released by
	// real instance state.
	if err := svc.CancelRelease(rel1.ID, now); err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 3*time.Second)
	st = snapshotStore(t, dir)
	watch.check(t, st)
	if got := releaseOf(st, rel1.ID).Status; got != "cancelled" {
		t.Fatalf("rel1 应已取消，实际 %s", got)
	}
	cancelPlanned := false
	for _, p := range plansOf(st) {
		if strings.Contains(p.Trigger, "取消发布 "+rel1.ID) {
			cancelPlanned = true
		}
	}
	if !cancelPlanned {
		t.Fatal("取消发布应触发一次协调重排并记录原因")
	}
	for _, g := range grantsOf(st) {
		if g.State == "held" {
			t.Fatalf("全部容量承诺都应释放，仍持有: %+v", g)
		}
	}
	// Every instance's fate is explicit: cancelled in-flight batches rolled
	// back to the baseline, succeeded batches keep their verified version.
	for _, b := range batchesOf(st) {
		for _, m := range b.Members {
			if m.UpdateState == "in_progress" || m.UpdateState == "rollback_in_progress" {
				t.Fatalf("实例 %s 仍处于中间状态 %s", m.InstanceID, m.UpdateState)
			}
		}
	}
	for _, inst := range allInstances(st) {
		if inst.ID == "inst-7" && inst.CurrentVersion != "v1.0.0" {
			t.Fatalf("回退实例 inst-7 应在原版本，实际 %s", inst.CurrentVersion)
		}
	}
}

// A release whose deadline cannot be met gets a concrete conflict naming
// the bottleneck zone and the release holding the capacity.
func TestDeadlineConflictExplained(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	watch := newCapacityWatch()

	if err := svc.SetZoneCapacity("zone-a", map[string]int{}, 1, now); err != nil {
		t.Fatal(err)
	}
	addPayInstances(t, svc, now)

	rel1, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Priority: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 100*time.Millisecond)
	watch.check(t, snapshotStore(t, dir))

	// rel2's two batches must queue behind rel1's zone-a slots; a 250ms
	// deadline is impossible and the plan must say exactly why.
	deadline := now.Add(250 * time.Millisecond).Format(time.RFC3339)
	rel2, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v1.5.0", BatchSize: 1, Priority: 0,
		Deadline: deadline,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	st := snapshotStore(t, dir)
	var plan *store.PlanRecord
	for i, p := range plansOf(st) {
		if strings.Contains(p.Trigger, rel2.ID) {
			pp := plansOf(st)[i]
			plan = &pp
		}
	}
	if plan == nil {
		t.Fatal("创建发布应产生协调计划")
	}
	var miss *store.PlanConflict
	for i, c := range plan.Conflicts {
		if c.DeadlineMiss && c.ReleaseID == rel2.ID {
			cc := plan.Conflicts[i]
			miss = &cc
		}
	}
	if miss == nil {
		t.Fatalf("应报告无法按期完成的容量冲突: %+v", plan.Conflicts)
	}
	if !strings.Contains(miss.Reason, "最迟完成时间") || !strings.Contains(miss.Reason, rel1.ID) {
		t.Fatalf("冲突原因应说明最迟完成时间与占用方 %s: %s", rel1.ID, miss.Reason)
	}

	// The plan history (and its reordering reasons) survives a reopen.
	st2 := snapshotStore(t, dir)
	if len(plansOf(st2)) == 0 {
		t.Fatal("协调计划应持久化，重开后仍可查询")
	}
	runUntil(t, svc, &now, 5*time.Second)
	watch.check(t, snapshotStore(t, dir))
}
