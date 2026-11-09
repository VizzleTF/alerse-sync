//go:build integration

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/login"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// call sends a JSON request to the login API and returns the status and body.
func call(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, serverURL+path, bytes.NewReader(data))
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

func testLoginID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// TestLoginOnPostgres sets a login before any device synced, joins a second
// device with it, and checks that clearing the chain removes the login.
func TestLoginOnPostgres(t *testing.T) {
	key := newChain(t)
	token := auth.NewToken(key, time.Now())
	id := testLoginID("e2e@example.com")
	authKey := bytes.Repeat([]byte{4}, 32)
	wrapped := bytes.Repeat([]byte{5}, 60)
	set := map[string]any{
		"id": id, "scheme": login.Scheme, "kdf": login.DefaultKDF,
		"salt": bytes.Repeat([]byte{6}, 32), "auth": authKey, "secret": wrapped,
	}
	if status, body := call(t, "PUT", "/login", token, set); status != 204 {
		t.Fatalf("set: HTTP %d: %s", status, body)
	}

	// The chain exists with its permanent entities: a bookmark commits.
	a := &device{t: t, key: key, guid: "a"}
	a.bookmarks()
	if rsp := a.commit(bookmark("c1", "Arcomium")); rsp[0].GetResponseType() != pb.CommitResponse_SUCCESS {
		t.Fatalf("commit: %v", rsp[0].GetResponseType())
	}

	// Another chain cannot take the login.
	if status, _ := call(t, "PUT", "/login", auth.NewToken(newChain(t), time.Now()), set); status != 409 {
		t.Fatalf("taken login: HTTP %d", status)
	}

	status, body := call(t, "POST", "/login/unlock", "", map[string]any{"id": id, "auth": make([]byte, 32)})
	if status != 401 {
		t.Fatalf("wrong password: HTTP %d: %s", status, body)
	}
	status, body = call(t, "POST", "/login/unlock", "", map[string]any{"id": id, "auth": authKey})
	var unlocked struct{ Secret []byte }
	if status != 200 || json.Unmarshal(body, &unlocked) != nil || !bytes.Equal(unlocked.Secret, wrapped) {
		t.Fatalf("unlock: HTTP %d: %s", status, body)
	}
	// With the secret, the new device derives the chain's key and syncs.
	b := &device{t: t, key: key, guid: "b"}
	found := false
	for _, e := range b.bookmarks() {
		found = found || e.GetNonUniqueName() == "Arcomium"
	}
	if !found {
		t.Fatal("the second device did not get the bookmark")
	}

	// Clearing the chain's data removes its login too.
	rsp := a.send(&pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_CLEAR_SERVER_DATA.Enum(),
		ClearServerData: &pb.ClearServerDataMessage{},
	})
	if rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		t.Fatalf("clear: %v", rsp.GetErrorCode())
	}
	if _, ok, err := st.Login(t.Context(), id); err != nil || ok {
		t.Fatalf("login after clear: %v %v", ok, err)
	}
}
