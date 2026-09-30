package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"releasectl/internal/api"
	"releasectl/internal/engine"
	"releasectl/internal/store"
)

// This drill platform never deploys anything: all "instances" are records on
// disk and every deploy/health/rollback action is a simulated delayed op.
func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "./data", "directory for state files")
	staticDir := flag.String("static", "../frontend/dist", "built frontend directory")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	svc := engine.NewService(st, engine.DefaultConfig())
	if err := svc.Seed(time.Now()); err != nil {
		log.Fatalf("seed: %v", err)
	}
	srv := api.NewServer(svc, st)

	go srv.Run(500 * time.Millisecond)

	static := resolveStaticDir(*staticDir)
	if static == "" {
		log.Printf("警告：未找到前端构建产物（尝试过 %s、frontend/dist、dist），页面将 404；请先在 frontend 目录执行 npm run build", *staticDir)
		static = *staticDir
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Routes())
	mux.Handle("/", http.FileServer(http.Dir(static)))

	log.Printf("软件发布演练平台监听 %s（数据目录 %s，前端目录 %s，全部为本地虚拟实例）", *addr, *dataDir, static)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// resolveStaticDir finds the built frontend no matter whether the server is
// started from the repo root or from backend/. Serving a missing directory
// used to be the silent cause of a blank page on first load.
func resolveStaticDir(flagVal string) string {
	for _, dir := range []string{flagVal, "frontend/dist", "../frontend/dist", "dist"} {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, "index.html")); err == nil && !info.IsDir() {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return dir
			}
			return abs
		}
	}
	return ""
}
