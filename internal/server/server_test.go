package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
	"github.com/VizzleTF/arcomium-sync/internal/protocol"
	"github.com/VizzleTF/arcomium-sync/internal/ratelimit"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

func newHandler(t *testing.T, perMinute int, health error) (http.Handler, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(Config{
		Service: protocol.New(memory.New(), nil, protocol.DefaultLimits),
		Events:  http.NotFoundHandler(),
		Limiter: ratelimit.New(perMinute),
		Health:  func(context.Context) error { return health },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h, auth.NewToken(key, time.Now())
}

func getUpdates(t *testing.T) []byte {
	t.Helper()
	data, err := proto.Marshal(&pb.ClientToServerMessage{
		Share:           proto.String(""),
		MessageContents: pb.ClientToServerMessage_GET_UPDATES.Enum(),
		GetUpdates: &pb.GetUpdatesMessage{FromProgressMarker: []*pb.DataTypeProgressMarker{
			{DataTypeId: proto.Int32(32904)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(data) //nolint:errcheck // writes to memory
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type request struct {
	path     string
	body     []byte
	token    string
	encoding string
	accept   string
}

func do(h http.Handler, r request) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, r.path, bytes.NewReader(r.body))
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	if r.encoding != "" {
		req.Header.Set("Content-Encoding", r.encoding)
	}
	if r.accept != "" {
		req.Header.Set("Accept-Encoding", r.accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// response decodes a protocol response, gunzipping it if needed.
func response(t *testing.T, rec *httptest.ResponseRecorder) *pb.ClientToServerResponse {
	t.Helper()
	body := io.Reader(rec.Body)
	if rec.Header().Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			t.Fatal(err)
		}
		body = gz
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	rsp := &pb.ClientToServerResponse{}
	if err := proto.Unmarshal(data, rsp); err != nil {
		t.Fatal(err)
	}
	return rsp
}

func TestCommand(t *testing.T) {
	h, token := newHandler(t, 0, nil)
	body := getUpdates(t)

	tests := []struct {
		name   string
		req    request
		status int
		gzip   bool
	}{
		{"plain", request{path: "/v2/command/?client=Chromium&client_id=x", body: body, token: token}, 200, false},
		{"no trailing slash", request{path: "/v2/command", body: body, token: token}, 200, false},
		// The response is too small to compress.
		{"gzip request", request{path: "/v2/command/", body: gzipped(t, body), token: token,
			encoding: "gzip", accept: "gzip, deflate"}, 200, false},
		{"no token", request{path: "/v2/command/", body: body}, 401, false},
		{"not protobuf", request{path: "/v2/command/", body: []byte("hello"), token: token}, 400, false},
		{"bad gzip", request{path: "/v2/command/", body: body, token: token, encoding: "gzip"}, 400, false},
		{"unknown encoding", request{path: "/v2/command/", body: body, token: token, encoding: "br"}, 400, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := do(h, test.req)
			if rec.Code != test.status {
				t.Fatalf("HTTP %d, want %d: %s", rec.Code, test.status, rec.Body)
			}
			if rec.Code != 200 {
				return
			}
			if gotGzip := rec.Header().Get("Content-Encoding") == "gzip"; gotGzip != test.gzip {
				t.Fatalf("gzip response %v, want %v", gotGzip, test.gzip)
			}
			if rsp := response(t, rec); rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
				t.Fatalf("error code %v", rsp.GetErrorCode())
			}
		})
	}
}

func TestWriteBody(t *testing.T) {
	large := bytes.Repeat([]byte("sync"), minGzipBytes)
	for _, test := range []struct {
		name   string
		data   []byte
		accept string
		gzip   bool
	}{
		{"large", large, "gzip", true},
		{"small", []byte("sync"), "gzip", false},
		{"not accepted", large, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("Accept-Encoding", test.accept)
			rec := httptest.NewRecorder()
			writeBody(rec, req, test.data)
			if got := rec.Header().Get("Content-Encoding") == "gzip"; got != test.gzip {
				t.Fatalf("gzip %v, want %v", got, test.gzip)
			}
			body := io.Reader(rec.Body)
			if test.gzip {
				gz, err := gzip.NewReader(body)
				if err != nil {
					t.Fatal(err)
				}
				body = gz
			}
			if got, _ := io.ReadAll(body); !bytes.Equal(got, test.data) {
				t.Fatal("body changed")
			}
		})
	}
}

func TestCommandThrottled(t *testing.T) {
	h, token := newHandler(t, 1, nil)
	req := request{path: "/v2/command/", body: getUpdates(t), token: token}
	if rsp := response(t, do(h, req)); rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		t.Fatalf("first request: %v", rsp.GetErrorCode())
	}
	rsp := response(t, do(h, req))
	if rsp.GetErrorCode() != pb.SyncEnums_THROTTLED || rsp.GetClientCommand().GetThrottleDelaySeconds() < 1 {
		t.Fatalf("second request: %v, delay %d", rsp.GetErrorCode(), rsp.GetClientCommand().GetThrottleDelaySeconds())
	}
}

func TestHealth(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{{nil, 200}, {errors.New("down"), 503}} {
		h, _ := newHandler(t, 0, test.err)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != test.status {
			t.Errorf("health with %v: HTTP %d, want %d", test.err, rec.Code, test.status)
		}
	}
}
