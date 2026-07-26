// Command server is a deployable jsingo service.
//
// It exists to make the deployment manifests concrete: the probe endpoints in
// deploy/ assume this shape, and the Dockerfile builds this binary.
//
// The interesting part is the health model. Liveness and readiness answer
// different questions, and collapsing them turns a recoverable blip into an
// outage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DiyRex/jsingo"
	"github.com/DiyRex/jsingo/examples/readability"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	shutdownGrace := flag.Duration("shutdown-grace", 20*time.Second, "graceful shutdown budget")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*addr, *shutdownGrace, log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(addr string, grace time.Duration, log *slog.Logger) error {
	// Signals are handled before the sidecar starts, so a Ctrl-C during a slow
	// startup still tears the child down instead of orphaning it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := jsingo.New(ctx,
		jsingo.WithModule(readability.Module),
		jsingo.WithLogger(log),
		// The container sets the real limit; this makes the runtime fail
		// sooner rather than being OOM-killed mid-request.
		jsingo.WithMaxHeapMB(384),
		// A reply cap distinct from the frame size: a small request should not
		// be able to elicit an enormous reply.
		jsingo.WithMaxReplyBytes(8<<20),
	)
	if err != nil {
		return fmt.Errorf("start sidecar: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := rt.Close(closeCtx); err != nil {
			log.Error("sidecar shutdown", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(rt, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "runtime", rt.Stats().Runtime)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

func routes(rt *jsingo.Runtime, log *slog.Logger) http.Handler {
	client := readability.New(rt)
	mux := http.NewServeMux()

	// Liveness: terminal failures only.
	//
	// A sidecar respawn takes a few hundred milliseconds. Failing liveness for
	// that would restart the whole pod over a routine event. Only an exhausted
	// restart budget means the process will never recover on its own.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := rt.Err(); errors.Is(err, jsingo.ErrSidecarUnrecoverable) {
			http.Error(w, "sidecar unrecoverable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	// Readiness: can this instance serve right now?
	//
	// Fails during a respawn so the load balancer routes elsewhere for the few
	// hundred milliseconds it takes, instead of dropping requests.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := rt.Ping(ctx); err != nil {
			http.Error(w, "sidecar not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
	})

	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, rt.Stats())
	})

	mux.HandleFunc("POST /extract", func(w http.ResponseWriter, r *http.Request) {
		var req readability.ParseRequest
		// Bound the request body: the sidecar's frame limit is a backstop, not
		// the place to reject an oversized upload.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}

		article, err := client.Parse(r.Context(), req)
		if err != nil {
			writeCallError(w, log, err)
			return
		}
		writeJSON(w, http.StatusOK, article)
	})

	return mux
}

// writeCallError maps a sidecar failure onto an HTTP status.
//
// The mapping matters operationally: a page with no article is a 422, not a
// 500, and must not page anyone. A restarting sidecar is a 503 with
// Retry-After so a client backs off instead of hammering.
func writeCallError(w http.ResponseWriter, log *slog.Logger, err error) {
	var he *jsingo.HandlerError
	if errors.As(err, &he) {
		switch he.Code {
		case jsingo.CodeNotFound:
			http.Error(w, "no extractable article", http.StatusUnprocessableEntity)
			return
		case jsingo.CodeInvalidArgument:
			http.Error(w, he.Message, http.StatusBadRequest)
			return
		}
		// The JS stack goes to the log, never to the client: it names internal
		// paths and dependency versions.
		log.Error("handler failed", "method", he.Method, "code", he.Code.String(),
			"message", he.Message, "stack", he.Stack)
		http.Error(w, "extraction failed", http.StatusInternalServerError)
		return
	}

	if jsingo.Retryable(err) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "sidecar restarting", http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, context.Canceled) {
		// The client hung up; nothing to send.
		return
	}
	log.Error("call failed", "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
