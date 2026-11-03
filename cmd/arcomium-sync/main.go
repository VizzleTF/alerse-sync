// Command arcomium-sync is the sync server of Arcomium. It is configured with
// environment variables; see README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VizzleTF/arcomium-sync/internal/events"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/server"
	"github.com/VizzleTF/arcomium-sync/internal/store/postgres"
)

// version is set at build time.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

type config struct {
	databaseURL     string
	port            int
	ratePerMinute   int
	maxEntities     int
	maxChains       int
	maxConcurrent   int
	maxEventStreams int
	maxStreamsTotal int
}

func loadConfig() (config, error) {
	c := config{databaseURL: os.Getenv("DATABASE_URL")}
	if c.databaseURL == "" {
		return c, errors.New("DATABASE_URL is not set")
	}
	for _, v := range []struct {
		name  string
		value *int
		def   int
	}{
		{"PORT", &c.port, 8295},
		{"RATE_LIMIT_PER_MINUTE", &c.ratePerMinute, 600},
		{"MAX_ENTITIES", &c.maxEntities, int(protocol.DefaultLimits.MaxEntities)},
		{"MAX_CHAINS", &c.maxChains, 0},
		{"MAX_CONCURRENT_REQUESTS", &c.maxConcurrent, 64},
		{"MAX_EVENT_STREAMS", &c.maxEventStreams, 32},
		{"MAX_EVENT_STREAMS_TOTAL", &c.maxStreamsTotal, 10000},
	} {
		*v.value = v.def
		if s := os.Getenv(v.name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return c, fmt.Errorf("%s=%q: want a number of 0 or more", v.name, s)
			}
			*v.value = n
		}
	}
	return c, nil
}

func logLevel() slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return level
}

func run(ctx context.Context, log *slog.Logger) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, c.databaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	st := postgres.New(pool)
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("database schema: %w", err)
	}

	hub := events.New(pool, log, c.maxEventStreams, c.maxStreamsTotal)
	go hub.Listen(ctx)

	limits := protocol.DefaultLimits
	limits.MaxEntities = int64(c.maxEntities)
	limits.MaxChains = int64(c.maxChains)
	handler := server.Handler(server.Config{
		Service: protocol.New(st, hub, limits),
		Events:  hub,
		Limiter: ratelimit.New(c.ratePerMinute),
		Health:  st.Ping,
		Log:     log,

		MaxConcurrent: c.maxConcurrent,
	})

	// Requests share a context that ends at shutdown, so event streams end
	// and Shutdown does not wait for them.
	requests, endRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer endRequests()
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", c.port),
		Handler: handler,
		// No ReadTimeout: it would cut event streams. The command handler
		// sets its own deadline.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return requests },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	failed := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "version", version)
		failed <- srv.ListenAndServe()
	}()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	endRequests()
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
