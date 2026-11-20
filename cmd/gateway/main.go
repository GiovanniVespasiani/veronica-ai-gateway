// Command gateway runs the Veronica AI gateway.
//
// Phase A: the production session resolver is "unavailable", so the binary
// refuses to call the provider until a truthful conversation identity exists.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/veronica/ai-gateway/internal/config"
	"github.com/veronica/ai-gateway/internal/logging"
	"github.com/veronica/ai-gateway/internal/server"
	"github.com/veronica/ai-gateway/internal/upstream"
)

func main() {
	cfg, err := config.Production()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// No-op unless the binary was built with `-tags testmode`.
	cfg, err = testOverride(cfg)
	if err != nil {
		log.Fatalf("test override: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("configuration error after override: %v", err)
	}

	logger := logging.New(os.Stdout)

	resolver, err := buildResolver(cfg)
	if err != nil {
		log.Fatalf("session resolver: %v", err)
	}

	client, err := upstream.New(cfg)
	if err != nil {
		log.Fatalf("upstream client: %v", err)
	}

	srv, err := server.New(cfg, logger, client, resolver)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	sugared := func(rec logging.AccessRecord) {
		rec.Event = "startup"
		logger.Write(rec)
	}
	sugared(logging.AccessRecord{
		Level:   "info",
		Message: "gateway starting",
	})

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		// No WriteTimeout: responses stream for up to TotalTimeout and are
		// bounded by per-write deadlines instead.
	}

	go func() {
		logger.Write(logging.AccessRecord{
			Level:   "info",
			Event:   "startup",
			Message: "listening",
			Model:   cfg.Model,
		})
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	logger.Write(logging.AccessRecord{Level: "info", Event: "shutdown"})
}
