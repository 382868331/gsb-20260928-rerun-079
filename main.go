// Command releaselab runs the local software release drill platform.
// It never deploys anything real: instances are local virtual records.
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"

	"releaselab/internal/api"
	"releaselab/internal/engine"
	"releaselab/internal/store"
)

//go:embed all:web/dist
var webAssets embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "./data", "directory for state files")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	eng := engine.New(st)
	eng.Start()
	defer eng.Stop()

	dist, err := fs.Sub(webAssets, "web/dist")
	if err != nil {
		log.Fatalf("web assets: %v", err)
	}
	srv := api.New(eng, dist)
	log.Printf("release drill platform listening on %s (state in %s)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
