package engine_test

import (
	"strings"
	"testing"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// A plan whose batches could never fit the agreed floor is refused at
// creation with the exact capacity condition; spreading the same release
// across zones makes it schedulable.
func TestCreateReleaseRejectsPlanThatAlwaysBreachesFloor(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	if _, err := svc.AddPolicy(store.CapacityPolicy{
		ServiceID: "svc-order", ZoneID: "zone-east", MinAvailable: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "rolling",
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err == nil || !strings.Contains(err.Error(), "容量下限") {
		t.Fatalf("concentrated plan must be rejected with the capacity condition, got %v", err)
	}
	// The same release spread across zones keeps two available per zone.
	rel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 2, Strategy: "zone_spread",
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatalf("zone-spread plan must be accepted, got %v", err)
	}
	batches := batchesOf(t, dir, rel.ID)
	if len(batches) != 3 {
		t.Fatalf("zone_spread must still produce 3 batches, got %d", len(batches))
	}
	// Every batch mixes zones so no zone loses more than one instance.
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, b := range batches {
			east := 0
			for _, m := range b.Members {
				if inst := findInstanceForTest(tx, m.InstanceID); inst != nil && inst.ZoneID == "zone-east" {
					east++
				}
			}
			if east > 1 {
				t.Fatalf("zone-spread batch %d takes %d east instances, want <= 1", b.Index, east)
			}
		}
	})
	runUntil(t, svc, &now, 8*time.Second)
	if r := releaseByID(t, dir, rel.ID); r.Status != "succeeded" {
		t.Fatalf("zone-spread release = %s, want succeeded", r.Status)
	}
}

func findInstanceForTest(tx *store.Tx, id string) *store.Instance {
	for i := range tx.Instances() {
		if tx.Instances()[i].ID == id {
			return &tx.Instances()[i]
		}
	}
	return nil
}

// Rolling back to a baseline that breaks a dependent's constraint is
// called out in the rollback assessment, and the completed batch keeps its
// version (the rollback never rewrites history).
func TestIncompatibleRollbackIsExplained(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	addInstances(t, svc, "svc-pay", 2, now)
	var payInsts []string
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, inst := range tx.Instances() {
			if inst.ServiceID == "svc-pay" {
				payInsts = append(payInsts, inst.ID)
			}
		}
	})
	if len(payInsts) != 2 {
		t.Fatalf("expected 2 pay instances, got %v", payInsts)
	}
	// Order v2 requires pay v2.x: rolling pay back to v1 afterwards is
	// incompatible with the order service that already moved.
	if _, err := svc.AddRule(store.CompatRule{
		ProviderID: "svc-pay", DependentID: "svc-order", DependentVersion: "v2.0.0",
		MinProviderVersion: "v2.0.0", MaxProviderVersion: "v2.9.9",
		Mode: "update_first", Note: "订单 v2 需要支付 v2.x",
	}, now); err != nil {
		t.Fatal(err)
	}
	// Move order to v2 first.
	orderRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-order", TargetVersion: "v2.0.0", BatchSize: 3,
		Failure: store.FailureSpec{Mode: "none"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)
	if r := releaseByID(t, dir, orderRel.ID); r.Status != "succeeded" {
		t.Fatalf("order release = %s, want succeeded", r.Status)
	}
	// Pay release: first instance updates fine, the second fails health and
	// the operator rolls back from the review pause.
	payRel, err := svc.CreateRelease(engine.CreateReleaseRequest{
		ServiceID: "svc-pay", TargetVersion: "v2.0.0", BatchSize: 1,
		Failure: store.FailureSpec{Mode: "instances", Instances: []string{payInsts[1]}, PauseOnFailure: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runUntil(t, svc, &now, 5*time.Second)
	if r := releaseByID(t, dir, payRel.ID); r.Status != "paused" {
		t.Fatalf("pay release = %s, want paused for review", r.Status)
	}
	if err := svc.ReviewRollback(payRel.ID, now); err != nil {
		t.Fatal(err)
	}
	r := releaseByID(t, dir, payRel.ID)
	if !strings.Contains(r.RollbackNote, "仍有 1 条依赖约束不满足") {
		t.Fatalf("rollback note must flag the incompatible baseline, got %q", r.RollbackNote)
	}
	runUntil(t, svc, &now, 5*time.Second)
	if r := releaseByID(t, dir, payRel.ID); r.Status != "rolled_back" {
		t.Fatalf("pay release = %s, want rolled_back", r.Status)
	}
	// The completed first batch keeps v2.0.0; only the failed batch
	// returned to the baseline.
	snapshotStore(t, dir).View(func(tx *store.Tx) {
		for _, inst := range tx.Instances() {
			if inst.ServiceID != "svc-pay" {
				continue
			}
			want := "v2.0.0"
			if inst.ID == payInsts[1] {
				want = "v1.0.0"
			}
			if inst.CurrentVersion != want {
				t.Fatalf("pay instance %s = %s, want %s", inst.ID, inst.CurrentVersion, want)
			}
		}
	})
}

// Plan comparison explains where each arrangement blocks: updating the
// dependent first is feasible, updating the provider first hits the
// dependency, and concentrated batches hit the capacity floor while
// zone-spread batches pass.
func TestComparePlansExplainsDependencyAndCapacity(t *testing.T) {
	dir := t.TempDir()
	svc := newService(t, dir)
	now := t0
	addInstances(t, svc, "svc-pay", 2, now)
	if _, err := svc.AddPolicy(store.CapacityPolicy{
		ServiceID: "svc-order", ZoneID: "zone-east", MinAvailable: 1,
	}, now); err != nil {
		t.Fatal(err)
	}
	step := func(serviceID, strategy string, size int) engine.PlanStep {
		return engine.PlanStep{ServiceID: serviceID, TargetVersion: "v2.0.0", Strategy: strategy, BatchSize: size}
	}
	res := svc.ComparePlans(engine.CompareRequest{Scenarios: []engine.CompareScenario{
		{Name: "先更新支付服务", Steps: []engine.PlanStep{step("svc-pay", "rolling", 1)}},
		{Name: "先更新订单服务", Steps: []engine.PlanStep{step("svc-order", "rolling", 2), step("svc-pay", "rolling", 1)}},
		{Name: "订单集中批次", Steps: []engine.PlanStep{step("svc-order", "rolling", 3)}},
		{Name: "订单分散区域", Steps: []engine.PlanStep{step("svc-order", "zone_spread", 3)}},
	}}, now)
	if len(res) != 4 {
		t.Fatalf("expected 4 scenario explanations, got %d", len(res))
	}
	byName := map[string]engine.ScenarioExplanation{}
	for _, sc := range res {
		byName[sc.Name] = sc
	}
	payFirst := byName["先更新支付服务"]
	if payFirst.Feasible || payFirst.BlockedStep != 0 {
		t.Fatalf("pay-first must block at step 1, got %+v", payFirst)
	}
	if !strings.Contains(payFirst.Summary, "依赖约束") {
		t.Fatalf("pay-first summary must name the dependency condition, got %q", payFirst.Summary)
	}
	orderFirst := byName["先更新订单服务"]
	if !orderFirst.Feasible {
		t.Fatalf("order-first must be feasible, got %+v", orderFirst)
	}
	concentrated := byName["订单集中批次"]
	if concentrated.Feasible || !strings.Contains(concentrated.Summary, "容量条件") {
		t.Fatalf("concentrated batches must block on the capacity condition, got %+v", concentrated)
	}
	spread := byName["订单分散区域"]
	if !spread.Feasible {
		t.Fatalf("zone-spread must pass the same floor, got %+v", spread)
	}
	// Blocked scenarios stop at the first blocked step and skip the rest.
	if concentrated.Steps[0].Feasible {
		t.Fatal("the concentrated step itself must be marked infeasible")
	}
}
