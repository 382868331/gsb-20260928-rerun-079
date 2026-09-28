package main

import (
	"flag"
	"log"
	"net/http"
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

	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Routes())
	mux.Handle("/", http.FileServer(http.Dir(*staticDir)))

	log.Printf("软件发布演练平台监听 %s（数据目录 %s，全部为本地虚拟实例）", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
