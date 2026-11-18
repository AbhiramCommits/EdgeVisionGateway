package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/config"
)

// The full gateway wiring (store, detector, queue, workers, REST + gRPC) is
// added in the ingestion step. For now this serves health endpoints so the
// scaffold is runnable and `make build` proves the module compiles.
func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	slog.Info("gateway listening", "addr", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
