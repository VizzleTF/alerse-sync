package server

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/account"
	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/credential"
	"github.com/VizzleTF/arcomium-sync/internal/metrics"
	"github.com/VizzleTF/arcomium-sync/internal/store"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

const testAccount = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// TestV3Chain syncs a chain with a device token, and checks that the chain's
// own key no longer reaches it over /v2.
func TestV3Chain(t *testing.T) {
	var logs bytes.Buffer
	st := memory.New()
	c := baseConfig(st)
	c.Accounts = account.New(account.Config{
		Store: st, Hash: credential.Hash{Time: 1, Memory: 64, Threads: 1},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	c.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	c.AccessLog = true
	c.Metrics = metrics.New()
	h := Handler(c)

	_, device, _ := ed25519.GenerateKey(nil)
	_, chainKey, _ := ed25519.GenerateKey(nil)
	chainID := auth.ChainID(chainKey.Public().(ed25519.PublicKey))
	err := st.CreateAccount(t.Context(), store.Account{ID: testAccount, Kind: store.AccountKindSK},
		store.Envelope{ID: strings.Repeat("1", 64), Scheme: account.SchemeSK},
		store.Device{PublicKey: device.Public().(ed25519.PublicKey), Name: "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	deviceToken := auth.NewDeviceToken(device, testAccount, time.Now())

	rec := do(h, request{path: "/v3/chains/" + chainID + "/command/", body: getUpdates(t), token: deviceToken})
	if rec.Code != http.StatusOK || response(t, rec).GetErrorCode() != pb.SyncEnums_SUCCESS {
		t.Fatalf("v3 GetUpdates: HTTP %d: %s", rec.Code, rec.Body)
	}
	if owner, ok, _ := st.ChainAccount(t.Context(), chainID); !ok || owner != testAccount {
		t.Fatal("the chain did not become the account's")
	}
	// The chain token of the same chain is refused now.
	rec = do(h, request{path: "/v2/command/", body: getUpdates(t), token: auth.NewToken(chainKey, time.Now())})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("v2 on an account's chain: HTTP %d", rec.Code)
	}
	// So is a chain token on /v3, and a device token on /v2.
	rec = do(h, request{path: "/v3/chains/" + chainID + "/command/", body: getUpdates(t), token: auth.NewToken(chainKey, time.Now())})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("chain token on v3: HTTP %d", rec.Code)
	}
	if rec := do(h, request{path: "/v2/command/", body: getUpdates(t), token: deviceToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("device token on v2: HTTP %d", rec.Code)
	}

	// Events of the chain.
	req := httptest.NewRequest(http.MethodGet, "/v3/chains/"+chainID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	events := httptest.NewRecorder()
	h.ServeHTTP(events, req)
	if events.Code != http.StatusOK || events.Body.String() != "stream" {
		t.Fatalf("v3 events: HTTP %d: %s", events.Code, events.Body)
	}

	out := logs.String()
	if strings.Contains(out, chainID) || !strings.Contains(out, `"path":"/v3/chains/`+chainID[:12]+`/command/"`) {
		t.Errorf("the access log should name the chain shortened: %s", out)
	}
	rec = httptest.NewRecorder()
	c.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := `arcomium_sync_requests_total{message="get_updates",result="SUCCESS",route="command"} 1`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("metrics lack %s", want)
	}
}

func TestLogPath(t *testing.T) {
	long := strings.Repeat("ab", 32)
	for path, want := range map[string]string{
		"/v2/command/":                     "/v2/command/",
		"/v3/chains/" + long + "/command/": "/v3/chains/abababababab/command/",
		"/v3/chains/" + long + "/events":   "/v3/chains/abababababab/events",
		"/v3/devices/" + long:              "/v3/devices/abababababab",
		"/v3/chains/short":                 "/v3/chains/short",
	} {
		if got := logPath(path); got != want {
			t.Errorf("logPath(%q) = %q, want %q", path, got, want)
		}
	}
}
