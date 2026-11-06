// Command arcomium-sync is the sync server of Arcomium. It is configured with
// environment variables; see README.md.
//
//	arcomium-sync               run the server
//	arcomium-sync healthcheck   exit 0 if the server on PORT is ready, for
//	                            container health checks without curl
//	arcomium-sync -version      print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VizzleTF/arcomium-sync/internal/clientip"
	"github.com/VizzleTF/arcomium-sync/internal/events"
	"github.com/VizzleTF/arcomium-sync/internal/metrics"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/retention"
	"github.com/VizzleTF/arcomium-sync/internal/server"
	"github.com/VizzleTF/arcomium-sync/internal/store/postgres"
)

// version is set at build time.
var version = "dev"

// shutdownTimeout is how long requests in progress may take to finish.
const shutdownTimeout = 30 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if flag.Arg(0) == "healthcheck" {
		os.Exit(healthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// healthcheck asks the server on PORT whether it is ready.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8295"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "not ready:", resp.Status)
		return 1
	}
	return 0
}

func logLevel() slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return level
}

func run(ctx context.Context, log *slog.Logger) error {
	c, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}

	poolConfig, err := pgxpool.ParseConfig(c.databaseURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	if !c.poolSizeSet {
		poolConfig.MaxConns = c.poolSize()
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	st := postgres.New(pool)
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("database schema: %w", err)
	}

	var m *metrics.Metrics
	if c.metricsAddr != "" {
		m = metrics.New()
	}

	hub := events.New(pool, log, c.maxEventStreams, c.maxStreamsTotal)
	hub.MaxPerAddr = c.maxStreamsPerAddr
	go hub.Listen(ctx)

	newChains := ratelimit.NewPer(c.newChainsPerAddrPerDay, 24*time.Hour)
	service := protocol.New(st, hub, c.limits,
		protocol.WithCreateGate(func(ctx context.Context) (bool, time.Duration) {
			key := clientip.Key(clientip.FromContext(ctx))
			if key == "" {
				return true, 0
			}
			ok, wait := newChains.Allow(key)
			if !ok {
				m.Limited("new_chains")
			}
			return ok, wait
		}),
		protocol.WithEvictionObserver(m.Evicted))

	chainLimiter := ratelimit.New(c.ratePerMinute)
	addrLimiter := ratelimit.New(c.ratePerAddrPerMinute)
	var ready atomic.Bool
	ready.Store(true)
	handler := server.Handler(server.Config{
		Service:     service,
		Events:      hub,
		Limiter:     chainLimiter,
		AddrLimiter: addrLimiter,
		ClientIP:    clientip.New(c.trustedProxies, c.clientIPHeader),
		ChainExists: func(ctx context.Context, chainID string) (bool, error) {
			_, ok, err := st.Chain(ctx, chainID)
			return ok, err
		},
		Health:        st.Ping,
		Ready:         ready.Load,
		Log:           log,
		AccessLog:     c.accessLog,
		Metrics:       m,
		MaxConcurrent: c.maxConcurrent,
		BodyTimeout:   c.bodyTimeout,
	})

	go retention.Job{
		Store:    st,
		Policy:   c.retention,
		Interval: c.retentionInterval,
		Log:      log,
		Done:     m.Collected,
	}.Run(ctx)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", c.port),
		Handler: handler,
		// No ReadTimeout: it would cut event streams. The command handler
		// sets its own deadlines.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	// Event streams never end on their own; Shutdown ends them first and
	// then waits for the protocol requests in progress.
	srv.RegisterOnShutdown(hub.Close)

	failed := make(chan error, 2)
	go func() {
		log.Info("listening", "addr", srv.Addr, "version", version)
		failed <- srv.ListenAndServe()
	}()
	var metricsSrv *http.Server
	if m != nil {
		m.Pool(pool)
		m.Gauge("event_streams", "Open event streams on this instance.", func() float64 { return float64(hub.Streams()) })
		m.Gauge("rate_limited_keys", "Chains and addresses the rate limiters track.", func() float64 {
			return float64(chainLimiter.Keys() + addrLimiter.Keys() + newChains.Keys())
		})
		m.Gauge("chains", "Sync chains on the server.", func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			n, _ := st.CountChains(ctx)
			return float64(n)
		})
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", m.Handler())
		metricsSrv = &http.Server{Addr: c.metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("serving metrics", "addr", c.metricsAddr)
			failed <- metricsSrv.ListenAndServe()
		}()
	}

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	// Load balancers see /readyz fail and stop sending requests before the
	// server stops taking them.
	ready.Store(false)
	log.Info("shutting down", "delay", c.shutdownDelay)
	time.Sleep(c.shutdownDelay)
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if metricsSrv != nil {
		defer metricsSrv.Shutdown(shutdown) //nolint:errcheck // metrics end with the process
	}
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
