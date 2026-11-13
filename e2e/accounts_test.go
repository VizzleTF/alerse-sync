//go:build integration

package e2e

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"golang.org/x/crypto/scrypt"
	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/account"
	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/credential"
	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// skKeys is what a client derives from the 24 words and the password, as
// docs/protocol.md describes for sk-v1.
type skKeys struct {
	id   string
	auth []byte
	wrap []byte
}

func hkdf32(t *testing.T, secret []byte, info string) []byte {
	t.Helper()
	key, err := hkdf.Key(sha256.New, secret, nil, info, 32)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func deriveSK(t *testing.T, sk []byte, password string, salt []byte, kdf credential.KDF) skKeys {
	t.Helper()
	pwkey, err := scrypt.Key([]byte(password), salt, int(kdf.N), int(kdf.R), int(kdf.P), 32)
	if err != nil {
		t.Fatal(err)
	}
	master := hkdf32(t, append(pwkey, hkdf32(t, sk, "alerse-sk-mix")...), "alerse-sk-v1")
	return skKeys{
		id:   hex.EncodeToString(hkdf32(t, sk, "alerse-sk-id")),
		auth: hkdf32(t, master, "alerse-sk-auth"),
		wrap: hkdf32(t, master, "alerse-sk-wrap"),
	}
}

func (k skKeys) gcm(t *testing.T) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(k.wrap)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return gcm
}

// seal wraps the account key: 0x01 | nonce | AES-256-GCM, bound to the id.
func (k skKeys) seal(t *testing.T, accountKey []byte) []byte {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	out := append([]byte{0x01}, nonce...)
	return k.gcm(t).Seal(out, nonce, accountKey, []byte("alerse-sk-v1"+k.id))
}

func (k skKeys) open(t *testing.T, wrapped []byte) []byte {
	t.Helper()
	if len(wrapped) < 13 || wrapped[0] != 0x01 {
		t.Fatalf("wrapped key of %d bytes", len(wrapped))
	}
	key, err := k.gcm(t).Open(nil, wrapped[1:13], wrapped[13:], []byte("alerse-sk-v1"+k.id))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// v3 sends a JSON request to /v3 and returns the status and body.
func v3(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, baseURL+"/v3"+path, bytes.NewReader(data))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func publicKey(key ed25519.PrivateKey) []byte { return key.Public().(ed25519.PublicKey) }

// accountDevice is a device of an account that syncs chain over /v3.
func accountDevice(t *testing.T, key ed25519.PrivateKey, accountID, chainID, guid string) *device {
	return &device{
		t: t, guid: guid, url: baseURL + "/v3/chains/" + chainID,
		newToken: func() string { return auth.NewDeviceToken(key, accountID, time.Now()) },
	}
}

// TestAccountOnPostgres creates an account with the words and a password,
// signs a second device in, syncs a chain between them, removes the second
// device and deletes the account.
func TestAccountOnPostgres(t *testing.T) {
	sk := make([]byte, 32)
	accountKey := make([]byte, 32)
	salt := make([]byte, 32)
	for _, b := range [][]byte{sk, accountKey, salt} {
		_, _ = rand.Read(b)
	}
	const password = "correct horse battery staple"
	_, laptop, _ := ed25519.GenerateKey(nil)
	_, phone, _ := ed25519.GenerateKey(nil)

	// The first device creates the account.
	keys := deriveSK(t, sk, password, salt, credential.DefaultKDF)
	status, body := v3(t, "POST", "/account", "", map[string]any{
		"id": keys.id, "scheme": account.SchemeSK, "salt": salt, "kdf": credential.DefaultKDF,
		"auth": keys.auth, "wrapped": keys.seal(t, accountKey),
		"device": map[string]any{"public_key": publicKey(laptop), "name": "Laptop"},
	})
	var created struct{ Account string }
	if status != 201 || json.Unmarshal(body, &created) != nil {
		t.Fatalf("create: HTTP %d: %s", status, body)
	}

	// The second device has the words and the password.
	status, body = v3(t, "POST", "/unlock/params", "", map[string]any{"id": keys.id})
	var params struct {
		Scheme string
		Salt   []byte
		KDF    credential.KDF
	}
	if status != 200 || json.Unmarshal(body, &params) != nil || params.Scheme != account.SchemeSK {
		t.Fatalf("params: HTTP %d: %s", status, body)
	}
	if !params.KDF.Valid() {
		t.Fatalf("the server sent weak KDF parameters: %+v", params.KDF)
	}
	phoneKeys := deriveSK(t, sk, password, params.Salt, params.KDF)
	status, body = v3(t, "POST", "/unlock", "", map[string]any{
		"id": phoneKeys.id, "auth": phoneKeys.auth,
		"device": map[string]any{"public_key": publicKey(phone), "name": "Phone"},
	})
	var unlocked struct {
		Account string
		Wrapped []byte
	}
	if status != 200 || json.Unmarshal(body, &unlocked) != nil || unlocked.Account != created.Account {
		t.Fatalf("unlock: HTTP %d: %s", status, body)
	}
	if !bytes.Equal(phoneKeys.open(t, unlocked.Wrapped), accountKey) {
		t.Fatal("the phone did not get the account key")
	}

	// Both sync a chain of the account, and the phone hears of the laptop's
	// commit.
	chainID := randomHex(t)
	a := accountDevice(t, laptop, created.Account, chainID, "laptop")
	b := accountDevice(t, phone, created.Account, chainID, "phone")
	a.bookmarks()
	b.bookmarks()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := b.events(ctx)
	expectEvent(t, stream, "ready {}")
	if rsp := a.commit(bookmark("c1", "Alerse")); rsp[0].GetResponseType() != pb.CommitResponse_SUCCESS {
		t.Fatalf("commit: %v", rsp[0].GetResponseType())
	}
	expectEvent(t, stream, "changed {\"types\":[32904]}")
	if got := b.bookmarks(); len(got) != 1 || got[0].GetNonUniqueName() != "Alerse" {
		t.Fatalf("the phone got %d bookmarks", len(got))
	}

	// Another account cannot reach the chain.
	_, stranger, _ := ed25519.GenerateKey(nil)
	strangerSK := make([]byte, 32)
	_, _ = rand.Read(strangerSK)
	strangerKeys := deriveSK(t, strangerSK, password, salt, credential.KDF{Name: "scrypt", N: 1 << 15, R: 8, P: 1})
	status, body = v3(t, "POST", "/account", "", map[string]any{
		"id": strangerKeys.id, "scheme": account.SchemeSK, "salt": salt, "kdf": credential.KDF{Name: "scrypt", N: 1 << 15, R: 8, P: 1},
		"auth": strangerKeys.auth, "wrapped": strangerKeys.seal(t, accountKey),
		"device": map[string]any{"public_key": publicKey(stranger), "name": "Stranger"},
	})
	var other struct{ Account string }
	if status != 201 || json.Unmarshal(body, &other) != nil {
		t.Fatalf("second account: HTTP %d: %s", status, body)
	}
	if status := commandStatus(t, baseURL+"/v3/chains/"+chainID, auth.NewDeviceToken(stranger, other.Account, time.Now())); status != 403 {
		t.Fatalf("another account's device: HTTP %d", status)
	}
	// Nor can an account take over a chain that no account owns.
	oldChain := randomHex(t)
	if err := st.Create(t.Context(), oldChain, 0, func(store.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if status := commandStatus(t, baseURL+"/v3/chains/"+oldChain, auth.NewDeviceToken(stranger, other.Account, time.Now())); status != 403 {
		t.Fatalf("claim of an old chain: HTTP %d", status)
	}

	// The list, then the laptop removes the phone after the password.
	laptopToken := func() string { return auth.NewDeviceToken(laptop, created.Account, time.Now()) }
	status, body = v3(t, "GET", "/devices", laptopToken(), nil)
	var list struct {
		Devices []struct {
			ID, Name string
			Current  bool
		}
	}
	if status != 200 || json.Unmarshal(body, &list) != nil || len(list.Devices) != 2 ||
		list.Devices[0].Name != "Laptop" || !list.Devices[0].Current || list.Devices[1].Current {
		t.Fatalf("devices: HTTP %d: %s", status, body)
	}
	status, body = v3(t, "DELETE", "/devices/"+hex.EncodeToString(publicKey(phone)), laptopToken(),
		map[string]any{"auth": keys.auth})
	if status != 204 {
		t.Fatalf("remove the phone: HTTP %d: %s", status, body)
	}
	if status := commandStatus(t, b.url, b.newToken()); status != 401 {
		t.Fatalf("the removed phone: HTTP %d", status)
	}
	if done, err := st.Collect(t.Context(), store.Retention{
		Now: time.Now().Add(91 * 24 * time.Hour), RemovedDeviceTTL: 90 * 24 * time.Hour, Batch: 100,
	}); err != nil || done.RemovedDevices < 1 {
		t.Fatalf("collect removed devices: %+v %v", done, err)
	}

	// Deleting the account removes its chain with it.
	status, body = v3(t, "DELETE", "/account", laptopToken(), map[string]any{"auth": keys.auth})
	if status != 204 {
		t.Fatalf("delete: HTTP %d: %s", status, body)
	}
	if _, ok, err := st.Chain(t.Context(), chainID); err != nil || ok {
		t.Fatalf("the chain after the account: %v %v", ok, err)
	}
	if status := commandStatus(t, a.url, laptopToken()); status != 401 {
		t.Fatalf("a device of the deleted account: HTTP %d", status)
	}
	if _, ok, _ := st.Chain(t.Context(), oldChain); !ok {
		t.Fatal("the old chain went too")
	}
}

// commandStatus sends a GetUpdates and returns the HTTP status.
func commandStatus(t *testing.T, url, token string) int {
	t.Helper()
	body, _ := proto.Marshal(&pb.ClientToServerMessage{
		Share:           proto.String(""),
		MessageContents: pb.ClientToServerMessage_GET_UPDATES.Enum(),
		GetUpdates: &pb.GetUpdatesMessage{FromProgressMarker: []*pb.DataTypeProgressMarker{
			{DataTypeId: proto.Int32(bookmarkType)},
		}},
	})
	req, _ := http.NewRequest(http.MethodPost, url+"/command/", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
