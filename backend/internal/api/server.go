package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"releasectl/internal/engine"
	"releasectl/internal/store"
)

type Server struct {
	svc *engine.Service
	st  *store.Store

	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func NewServer(svc *engine.Service, st *store.Store) *Server {
	return &Server{svc: svc, st: st, subs: map[chan struct{}]struct{}{}}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/environment/init", s.handleInitEnvironment)
	mux.HandleFunc("POST /api/releases", s.handleCreateRelease)
	mux.HandleFunc("POST /api/releases/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/releases/{id}/review/continue", s.handleReviewContinue)
	mux.HandleFunc("POST /api/releases/{id}/review/shrink", s.handleReviewShrink)
	mux.HandleFunc("POST /api/releases/{id}/review/rollback", s.handleReviewRollback)
	mux.HandleFunc("POST /api/releases/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/releases/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/releases/{id}/finish", s.handleFinish)
	mux.HandleFunc("POST /api/versions", s.handleCreateVersion)
	mux.HandleFunc("POST /api/services/{id}/instances", s.handleAddInstance)
	mux.HandleFunc("POST /api/rules", s.handleAddRule)
	mux.HandleFunc("POST /api/zones", s.handleCreateZone)
	mux.HandleFunc("PUT /api/zones/{id}/capacity", s.handleSetZoneCapacity)
	mux.HandleFunc("POST /api/zones/{id}/failure", s.handleSetZoneFailure)
	mux.HandleFunc("POST /api/plans/compare", s.handleComparePlans)
	mux.HandleFunc("GET /api/capacity/compare", s.handleCompareCapacity)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	return withCORS(mux)
}

func (s *Server) snapshot() store.Snapshot {
	var snap store.Snapshot
	s.st.View(func(tx *store.Tx) {
		snap = store.Snapshot{
			// Non-nil slices: an empty drill must serialize as [], never
			// null, so a first open on empty data renders the empty state
			// instead of crashing the page on a null lookup.
			Versions:  append([]store.Version{}, tx.Versions()...),
			Services:  append([]store.Service{}, tx.Services()...),
			Zones:     append([]store.Zone{}, tx.Zones()...),
			Rules:     append([]store.CompatRule{}, tx.Rules()...),
			Instances: append([]store.Instance{}, tx.Instances()...),
			Releases:  append([]store.Release{}, tx.Releases()...),
			Batches:   append([]store.Batch{}, tx.Batches()...),
			Grants:    append([]store.CapacityGrant{}, tx.Grants()...),
			Ops:       append([]store.Op{}, tx.Ops()...),
			Events:    append([]store.Event{}, tx.Events()...),
			Plans:     append([]store.PlanRecord{}, tx.Plans()...),
		}
	})
	snap.Dependencies = append([]store.DependencyStatus{}, s.svc.DependencyStatuses()...)
	return snap
}

// Run drives the virtual clock and notifies SSE subscribers. Every state
// change is persisted by the engine before subscribers are woken, so a page
// refresh always shows real on-disk progress.
func (s *Server) Run(tick time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.svc.Engine().Tick(time.Now()); err != nil {
			log.Printf("tick error: %v", err)
		}
		s.broadcast()
	}
}

func (s *Server) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribe(ch chan struct{}) {
	s.mu.Lock()
	delete(s.subs, ch)
	close(ch)
	s.mu.Unlock()
}

func (s *Server) broadcast() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.snapshot())
}

func (s *Server) handleInitEnvironment(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureEnvironment(time.Now()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcast()
	writeJSON(w, s.snapshot())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := s.subscribe()
	defer s.unsubscribe(ch)
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(s.snapshot()))
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(s.snapshot()))
			flusher.Flush()
		}
	}
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	var req engine.CreateReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rel, err := s.svc.CreateRelease(req, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	writeJSON(w, rel)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.CancelRelease(r.PathValue("id"), time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.PauseForReview(r.PathValue("id"), time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleReviewContinue(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ReviewContinue(r.PathValue("id"), time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleReviewShrink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BatchSize int `json:"batchSize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.svc.ReviewShrink(r.PathValue("id"), req.BatchSize, time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleReviewRollback(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ReviewRollback(r.PathValue("id"), time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ResumeRollback(r.PathValue("id"), time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleFinish(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.FinishDrill(r.PathValue("id"), time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.svc.CreateVersion(req.ID, req.Name, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	writeJSON(w, v)
}

func (s *Server) handleAddInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.svc.AddInstance(r.PathValue("id"), time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	writeJSON(w, inst)
}

func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var rule store.CompatRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.svc.AddRule(rule, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	writeJSON(w, out)
}

func (s *Server) handleCreateZone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	z, err := s.svc.CreateZone(req.ID, req.Name, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	writeJSON(w, z)
}

func (s *Server) handleSetZoneCapacity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MinAvailable   map[string]int `json:"minAvailable"`
		SharedCapacity int            `json:"sharedCapacity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.svc.SetZoneCapacity(r.PathValue("id"), req.MinAvailable, req.SharedCapacity, time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleSetZoneFailure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Failed bool `json:"failed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.svc.SetZoneFailure(r.PathValue("id"), req.Failed, time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.broadcast()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleComparePlans(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServiceID string `json:"serviceId"`
		BatchSize int    `json:"batchSize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ServiceID == "" {
		writeErr(w, http.StatusBadRequest, "必须指定目标服务")
		return
	}
	plans, err := s.svc.ComparePlans(req.ServiceID, req.BatchSize)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"plans": plans})
}

// handleCompareCapacity serves the cross-release capacity comparison. It is
// a pure GET over the persisted snapshot: it never acquires commitments,
// never releases instances and never mutates release state.
func (s *Server) handleCompareCapacity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ids := make([]string, 0, 2)
	for _, key := range []string{"a", "b"} {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			ids = append(ids, v)
		}
	}
	cmp, err := s.svc.CompareReleaseCapacity(ids, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, cmp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(mustJSON(v))
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	w.Write(mustJSON(map[string]string{"error": msg}))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			return
		}
		next.ServeHTTP(w, r)
	})
}
