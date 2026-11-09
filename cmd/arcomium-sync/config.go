package main

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/clientip"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/server"
	"github.com/VizzleTF/arcomium-sync/internal/store"
)

const day = 24 * time.Hour

// config is the server's configuration, read from environment variables.
type config struct {
	databaseURL string
	poolSizeSet bool
	port        int
	metricsAddr string

	limits                 protocol.Limits
	ratePerMinute          int
	ratePerAddrPerMinute   int
	newChainsPerAddrPerDay int
	maxConcurrent          int
	loginAttemptsPerHour   int
	loginAttemptsPerAddr   int
	loginLookupsPerAddr    int
	loginChangesPerHour    int
	maxEventStreams        int
	maxStreamsPerAddr      int
	maxStreamsTotal        int
	bodyTimeout            time.Duration

	trustedProxies []netip.Prefix
	clientIPHeader string

	retention         store.Retention
	retentionInterval time.Duration

	accessLog     bool
	shutdownDelay time.Duration
}

// poolSize is the number of database connections when DATABASE_URL does not
// say: one per request handled at once, up to 32, and some for events and
// the retention job.
func (c config) poolSize() int32 {
	n := 32
	if c.maxConcurrent > 0 {
		n = min(c.maxConcurrent, 32)
	}
	return int32(max(n, 4) + 4)
}

func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		databaseURL:    getenv("DATABASE_URL"),
		metricsAddr:    getenv("METRICS_ADDR"),
		clientIPHeader: "X-Forwarded-For",
		limits:         protocol.DefaultLimits,
	}
	if c.databaseURL == "" {
		return c, errors.New("DATABASE_URL is not set")
	}
	c.poolSizeSet = strings.Contains(c.databaseURL, "pool_max_conns")
	if header := getenv("CLIENT_IP_HEADER"); header != "" {
		c.clientIPHeader = header
	}

	var errs []error
	env := func(name string) (string, bool) {
		value := getenv(name)
		return value, value != ""
	}
	number := func(name string, value *int, def int) {
		*value = def
		if s, ok := env(name); ok {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				errs = append(errs, fmt.Errorf("%s=%q: want a number of 0 or more", name, s))
				return
			}
			*value = n
		}
	}
	count := func(name string, value *int64, def int64) {
		var n int
		number(name, &n, int(def))
		*value = int64(n)
	}
	size := func(name string, value *int64, def int64) {
		*value = def
		if s, ok := env(name); ok {
			n, err := parseSize(s)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", name, s, err))
				return
			}
			*value = n
		}
	}
	duration := func(name string, value *time.Duration, def time.Duration) {
		*value = def
		if s, ok := env(name); ok {
			d, err := parseDuration(s)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", name, s, err))
				return
			}
			*value = d
		}
	}
	boolean := func(name string, value *bool, def bool) {
		*value = def
		if s, ok := env(name); ok {
			b, err := strconv.ParseBool(s)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: want true or false", name, s))
				return
			}
			*value = b
		}
	}

	number("PORT", &c.port, 8295)
	number("RATE_LIMIT_PER_MINUTE", &c.ratePerMinute, 600)
	number("RATE_LIMIT_PER_IP_PER_MINUTE", &c.ratePerAddrPerMinute, 1200)
	number("NEW_CHAINS_PER_IP_PER_DAY", &c.newChainsPerAddrPerDay, 10)
	number("LOGIN_ATTEMPTS_PER_HOUR", &c.loginAttemptsPerHour, 10)
	number("LOGIN_ATTEMPTS_PER_IP_PER_HOUR", &c.loginAttemptsPerAddr, 30)
	number("LOGIN_LOOKUPS_PER_IP_PER_HOUR", &c.loginLookupsPerAddr, 120)
	number("LOGIN_CHANGES_PER_HOUR", &c.loginChangesPerHour, 30)
	count("MAX_ENTITIES", &c.limits.MaxEntities, protocol.DefaultLimits.MaxEntities)
	size("MAX_BYTES_PER_CHAIN", &c.limits.MaxBytes, protocol.DefaultLimits.MaxBytes)
	count("MAX_HISTORY_ENTITIES", &c.limits.MaxHistoryEntities, protocol.DefaultLimits.MaxHistoryEntities)
	size("MAX_HISTORY_BYTES", &c.limits.MaxHistoryBytes, protocol.DefaultLimits.MaxHistoryBytes)
	count("MAX_SESSION_ENTITIES", &c.limits.MaxSessionEntities, protocol.DefaultLimits.MaxSessionEntities)
	count("MAX_CHAINS", &c.limits.MaxChains, 0)
	boolean("REQUIRE_CUSTOM_PASSPHRASE", &c.limits.RequireCustomPassphrase, false)
	number("MAX_CONCURRENT_REQUESTS", &c.maxConcurrent, 64)
	number("MAX_EVENT_STREAMS", &c.maxEventStreams, 32)
	number("MAX_EVENT_STREAMS_PER_IP", &c.maxStreamsPerAddr, 64)
	number("MAX_EVENT_STREAMS_TOTAL", &c.maxStreamsTotal, 10000)
	duration("BODY_READ_TIMEOUT", &c.bodyTimeout, server.DefaultBodyTimeout)
	duration("TOMBSTONE_TTL", &c.retention.TombstoneTTL, 60*day)
	duration("HISTORY_TTL", &c.retention.ExpiringTypesTTL, 90*day)
	duration("EMPTY_CHAIN_TTL", &c.retention.EmptyChainTTL, 7*day)
	duration("INACTIVE_CHAIN_TTL", &c.retention.InactiveChainTTL, 0)
	duration("RETENTION_INTERVAL", &c.retentionInterval, time.Hour)
	boolean("ACCESS_LOG", &c.accessLog, true)
	duration("SHUTDOWN_DELAY", &c.shutdownDelay, 0)

	if s, ok := env("TRUSTED_PROXY"); ok {
		prefixes, err := clientip.ParsePrefixes(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXY: %w", err))
		}
		c.trustedProxies = prefixes
	}
	c.retention.ExpiringTypes = protocol.ExpiringTypes
	c.retention.EmptyVersion = protocol.EmptyChainVersion
	return c, errors.Join(errs...)
}

// parseSize reads a size in bytes, with an optional unit: KiB, MiB or GiB
// (or KB, MB, GB, which mean the same here).
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		bytes  int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}}
	unit := int64(1)
	number := strings.TrimSpace(s)
	for _, u := range units {
		if rest, ok := strings.CutSuffix(number, u.suffix); ok {
			number, unit = strings.TrimSpace(rest), u.bytes
			break
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/unit {
		return 0, errors.New("want a size like 268435456, 256MiB or 1GiB")
	}
	return n * unit, nil
}

// parseDuration reads a duration like 90s or 2h, or a number of days like
// 30d. 0 turns off what it sets.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n >= 0 && n < 100_000 {
			return time.Duration(n) * day, nil
		}
	} else if s == "0" {
		return 0, nil
	} else if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d, nil
	}
	return 0, errors.New("want a duration like 30s, 2h or 30d")
}
