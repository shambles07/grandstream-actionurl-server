// Command gsactiond receives Grandstream Action URL events and stores them in
// SQLite, exposing the data over a read-only JSON API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shambles07/grandstream-actionurl-server/internal/server"
	"github.com/shambles07/grandstream-actionurl-server/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gsactiond:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", envOr("GSACTION_ADDR", ":8080"), "listen address")
		dbPath    = flag.String("db", envOr("GSACTION_DB", "gsactiond.db"), "SQLite database path")
		token     = flag.String("token", os.Getenv("GSACTION_TOKEN"), "shared token phones must send as token=... (recommended)")
		apiToken  = flag.String("api-token", os.Getenv("GSACTION_API_TOKEN"), "bearer token required on /api requests")
		tlsCert   = flag.String("tls-cert", os.Getenv("GSACTION_TLS_CERT"), "TLS certificate file (enables HTTPS with -tls-key)")
		tlsKey    = flag.String("tls-key", os.Getenv("GSACTION_TLS_KEY"), "TLS private key file")
		retention = flag.String("retention", envOr("GSACTION_RETENTION", "90d"), "delete events older than this (e.g. 30d, 720h); 0 keeps forever")
		logLevel  = flag.String("log-level", envOr("GSACTION_LOG_LEVEL", "info"), "debug, info, warn or error")
	)
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("-log-level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	var keep time.Duration
	if *retention != "0" && *retention != "" {
		d, err := server.ParseDuration(*retention)
		if err != nil {
			return fmt.Errorf("-retention: %w", err)
		}
		keep = d
	}
	if *token == "" {
		log.Warn("no -token set: any host that can reach this server can submit events")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()
	log.Info("database ready", "path", *dbPath)

	if keep > 0 {
		go pruneLoop(ctx, st, keep, log)
	}

	srv := server.New(server.Config{
		Addr:        *addr,
		IngestToken: *token,
		APIToken:    *apiToken,
		TLSCert:     *tlsCert,
		TLSKey:      *tlsKey,
	}, st, log)
	return srv.Serve(ctx)
}

func pruneLoop(ctx context.Context, st *store.Store, keep time.Duration, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		ev, calls, err := st.Prune(ctx, time.Now().Add(-keep))
		if err != nil {
			log.Error("prune", "err", err)
		} else if ev+calls > 0 {
			log.Info("pruned", "events", ev, "calls", calls)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
