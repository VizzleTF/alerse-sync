package main

import (
	"strings"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/protocol"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(env(map[string]string{"DATABASE_URL": "postgres://x/y"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.port != 8295 || c.limits != protocol.DefaultLimits || c.retention.TombstoneTTL != 60*day ||
		c.retention.ExpiringTypesTTL != 90*day || c.retentionInterval != time.Hour || !c.accessLog ||
		c.clientIPHeader != "X-Forwarded-For" || c.poolSizeSet || c.poolSize() != 36 ||
		c.newAccountsPerAddr != 10 || c.unlockAttemptsPerHour != 10 || c.maxDevicesPerAccount != 50 ||
		c.maxChainsPerAccount != 50 || c.maxAccounts != 0 || c.retention.RemovedDeviceTTL != 90*day {
		t.Fatalf("defaults: %+v", c)
	}
	if _, err := loadConfig(env(nil)); err == nil {
		t.Fatal("no DATABASE_URL accepted")
	}
}

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig(env(map[string]string{
		"DATABASE_URL":              "postgres://x/y?pool_max_conns=10",
		"MAX_BYTES_PER_CHAIN":       "1GiB",
		"MAX_HISTORY_BYTES":         "1000",
		"TOMBSTONE_TTL":             "30d",
		"INACTIVE_CHAIN_TTL":        "365d",
		"BODY_READ_TIMEOUT":         "5s",
		"REQUIRE_CUSTOM_PASSPHRASE": "true",
		"TRUSTED_PROXY":             "10.0.0.0/8, ::1",
		"CLIENT_IP_HEADER":          "CF-Connecting-IP",
		"HISTORY_TTL":               "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.limits.MaxBytes != 1<<30 || c.limits.MaxHistoryBytes != 1000 || c.retention.TombstoneTTL != 30*day ||
		c.retention.InactiveChainTTL != 365*day || c.bodyTimeout != 5*time.Second ||
		!c.limits.RequireCustomPassphrase || len(c.trustedProxies) != 2 ||
		c.clientIPHeader != "CF-Connecting-IP" || c.retention.ExpiringTypesTTL != 0 || !c.poolSizeSet {
		t.Fatalf("config: %+v", c)
	}

	_, err = loadConfig(env(map[string]string{
		"DATABASE_URL":        "postgres://x/y",
		"MAX_ENTITIES":        "-1",
		"MAX_BYTES_PER_CHAIN": "lots",
		"TOMBSTONE_TTL":       "soon",
		"ACCESS_LOG":          "maybe",
		"TRUSTED_PROXY":       "everyone",
	}))
	for _, name := range []string{"MAX_ENTITIES", "MAX_BYTES_PER_CHAIN", "TOMBSTONE_TTL", "ACCESS_LOG", "TRUSTED_PROXY"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("error %v does not name %s", err, name)
		}
	}
}

func TestParseSizeAndDuration(t *testing.T) {
	for s, want := range map[string]int64{"0": 0, "512": 512, "256MiB": 256 << 20, "2 GB": 2 << 30, "4KiB": 4096} {
		if got, err := parseSize(s); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "-1", "1TiB", "MiB"} {
		if _, err := parseSize(s); err == nil {
			t.Errorf("parseSize(%q) accepted", s)
		}
	}
	for s, want := range map[string]time.Duration{"0": 0, "90s": 90 * time.Second, "2h": 2 * time.Hour, "7d": 7 * day} {
		if got, err := parseDuration(s); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	for _, s := range []string{"", "-1h", "xd", "1y"} {
		if _, err := parseDuration(s); err == nil {
			t.Errorf("parseDuration(%q) accepted", s)
		}
	}
}
