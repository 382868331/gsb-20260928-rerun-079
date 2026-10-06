package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// HTTP flow for multi-release coordination: two services with concurrent
// releases, a shared zone capacity, a priority and a latest-finish deadline.
// The schedule endpoint must return the executable batch order and name the
// concrete capacity conflict when the deadline cannot be met.
func TestScheduleHTTPFlow(t *testing.T) {
	e := newTestEnv(t)

	code, body := e.do(t, http.MethodPut, "/api/zones/zone-a/capacity",
		`{"minAvailable":{},"sharedCapacity":1}`)
	if code != 202 {
		t.Fatalf("set capacity = %d %s", code, body)
	}
	code, body = e.do(t, http.MethodPost, "/api/versions", `{"id":"v1.5.0","name":"pay 1.5.0"}`)
	if code != 200 {
		t.Fatalf("create version = %d %s", code, body)
	}
	code, body = e.do(t, http.MethodPost, "/api/services/svc-pay/instances", "")
	if code != 200 {
		t.Fatalf("add instance = %d %s", code, body)
	}

	deadline := time.Now().Add(450 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	code, body = e.do(t, http.MethodPost, "/api/releases",
		`{"serviceId":"svc-order","targetVersion":"v2.0.0","batchSize":1,"strategy":"rolling","priority":1,"deadline":"`+deadline+`","failure":{"mode":"none"}}`)
	if code != 200 {
		t.Fatalf("create release A = %d %s", code, body)
	}
	code, body = e.do(t, http.MethodPost, "/api/releases",
		`{"serviceId":"svc-pay","targetVersion":"v1.5.0","batchSize":1,"strategy":"rolling","priority":9,"failure":{"mode":"none"}}`)
	if code != 200 {
		t.Fatalf("create release B = %d %s", code, body)
	}

	code, body = e.do(t, http.MethodGet, "/api/schedule", "")
	if code != 200 {
		t.Fatalf("schedule = %d %s", code, body)
	}
	var view struct {
		Queue []struct {
			ReleaseID string `json:"releaseId"`
			State     string `json:"state"`
		} `json:"queue"`
		Estimates []struct {
			ReleaseID string   `json:"releaseId"`
			Deadline  string   `json:"deadline"`
			OnTime    bool     `json:"onTime"`
			Conflicts []string `json:"conflicts"`
		} `json:"estimates"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Queue) == 0 {
		t.Fatal("schedule must list the executable batch order")
	}
	if len(view.Estimates) != 2 {
		t.Fatalf("estimates = %d, want 2", len(view.Estimates))
	}
	var withDeadline *struct {
		ReleaseID string   `json:"releaseId"`
		Deadline  string   `json:"deadline"`
		OnTime    bool     `json:"onTime"`
		Conflicts []string `json:"conflicts"`
	}
	for i := range view.Estimates {
		if view.Estimates[i].Deadline != "" {
			withDeadline = &view.Estimates[i]
		}
	}
	if withDeadline == nil {
		t.Fatal("estimate must echo the release deadline")
	}
	if withDeadline.OnTime {
		t.Fatal("450ms deadline for 6 batches must be reported as not achievable")
	}
	if len(withDeadline.Conflicts) == 0 || !strings.Contains(strings.Join(withDeadline.Conflicts, "；"), "故障区域 A") {
		t.Fatalf("deadline miss must name the concrete zone conflict: %+v", withDeadline.Conflicts)
	}

	// The snapshot carries the same schedule so the operations page updates
	// over SSE without a second request.
	code, body = e.do(t, http.MethodGet, "/api/state", "")
	if code != 200 {
		t.Fatalf("state = %d", code)
	}
	if !strings.Contains(string(body), `"schedule"`) || !strings.Contains(string(body), `"queue"`) {
		t.Fatal("snapshot must embed the coordination schedule")
	}
	if !strings.Contains(string(body), `"priority":9`) {
		t.Fatal("snapshot releases must expose the operator-set priority")
	}
}
