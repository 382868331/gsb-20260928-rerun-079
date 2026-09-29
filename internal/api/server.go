package api

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"releaselab/internal/engine"
)

type Server struct {
	eng    *engine.Engine
	mux    *http.ServeMux
	static fs.FS
}

func New(eng *engine.Engine, static fs.FS) *Server {
	s := &Server{eng: eng, static: static}
	m := http.NewServeMux()
	m.HandleFunc("GET /api/state", s.getState)
	m.HandleFunc("POST /api/setup", s.setup)
	m.HandleFunc("POST /api/releases", s.createRelease)
	m.HandleFunc("POST /api/releases/{id}/cancel", s.cancel)
	m.HandleFunc("POST /api/pause", s.pause)
	m.HandleFunc("POST /api/resume", s.resume)
	s.mux = m
	return s
}

func (s *Server) Handler() http.Handler {
	if s.static == nil {
		return s.mux
	}
	fileServer := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.mux.ServeHTTP(w, r)
			return
		}
		// SPA fallback to index.html for client routes.
		if _, err := fs.Stat(s.static, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	snap := s.eng.SnapshotAt(time.Now().UnixMilli())
	writeJSON(w, snap)
}

type setupReq struct {
	BaseVersion   string      `json:"base_version"`
	TargetVersion string      `json:"target_version"`
	InstanceCount int         `json:"instance_count"`
	FailUpdateIdx []int       `json:"fail_update_idx"`
	UnhealthyIdx  []int       `json:"unhealthy_idx"`
	ProbeDelayMS  map[int]int `json:"probe_delay_ms"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req setupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.eng.ResetAndSeed(engine.SetupInput{
		BaseVersion:   req.BaseVersion,
		TargetVersion: req.TargetVersion,
		InstanceCount: req.InstanceCount,
		FailUpdateIdx: req.FailUpdateIdx,
		UnhealthyIdx:  req.UnhealthyIdx,
		ProbeDelayMS:  req.ProbeDelayMS,
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

type releaseReq struct {
	TargetVersionID string `json:"target_version_id"`
	BatchPercent    []int  `json:"batch_percent"`
	HealthWaitMS    int64  `json:"health_wait_ms"`
	ActionMS        int64  `json:"action_ms"`
}

func (s *Server) createRelease(w http.ResponseWriter, r *http.Request) {
	var req releaseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.HealthWaitMS == 0 {
		req.HealthWaitMS = 2000
	}
	if req.ActionMS == 0 {
		req.ActionMS = 800
	}
	id, err := s.eng.CreateRelease(req.TargetVersionID, req.BatchPercent, req.HealthWaitMS, req.ActionMS)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"id": id})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	s.eng.SetPaused(true)
	writeJSON(w, map[string]string{"status": "paused"})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	s.eng.SetPaused(false)
	writeJSON(w, map[string]string{"status": "running"})
}
