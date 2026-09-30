package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	mux.HandleFunc("POST /api/releases", s.handleCreateRelease)
	mux.HandleFunc("POST /api/releases/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/releases/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/releases/{id}/finish", s.handleFinish)
	mux.HandleFunc("POST /api/versions", s.handleCreateVersion)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	return withCORS(mux)
}

func (s *Server) snapshot() store.Snapshot {
	var snap store.Snapshot
	s.st.View(func(tx *store.Tx) {
		snap = store.Snapshot{
			Versions:  append([]store.Version(nil), tx.Versions()...),
			Instances: append([]store.Instance(nil), tx.Instances()...),
			Releases:  append([]store.Release(nil), tx.Releases()...),
			Batches:   append([]store.Batch(nil), tx.Batches()...),
			Ops:       append([]store.Op(nil), tx.Ops()...),
			Events:    append([]store.Event(nil), tx.Events()...),
		}
	})
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
