// Package events tells a sync chain's devices right away when another device
// commits, so they fetch the change instead of waiting for their next poll.
//
// Devices hold a Server-Sent Events stream open (GET /v2/events). A commit
// publishes the chain, the committing device and the changed data types.
// With Postgres the event goes through NOTIFY, so every server instance
// behind a load balancer delivers it to the devices connected to it.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VizzleTF/arcomium-sync/internal/auth"
)

const (
	channel           = "arcomium_sync"
	heartbeatInterval = 25 * time.Second
	retryInterval     = 5 * time.Second
	// writeTimeout drops a device that stops reading.
	writeTimeout = 30 * time.Second
)

// event is what instances send each other.
type event struct {
	Chain string `json:"c"`
	// Origin is the cache GUID of the committing device, which already has
	// the change.
	Origin    string  `json:"o,omitempty"`
	DataTypes []int32 `json:"t"`
}

// Hub delivers events to the devices connected to this instance.
type Hub struct {
	pool        *pgxpool.Pool // nil: deliver within this instance only
	log         *slog.Logger
	maxPerChain int
	maxTotal    int

	mu          sync.Mutex
	subscribers map[string]map[*subscriber]struct{}
	total       int
}

// New returns a hub. With pool set, Notify goes through Postgres NOTIFY and
// Listen must run. maxPerChain limits the streams of one chain, maxTotal
// those of the instance.
func New(pool *pgxpool.Pool, log *slog.Logger, maxPerChain, maxTotal int) *Hub {
	return &Hub{
		pool: pool, log: log, maxPerChain: maxPerChain, maxTotal: maxTotal,
		subscribers: map[string]map[*subscriber]struct{}{},
	}
}

// Notify announces changed data types of a chain to its devices other than
// origin.
func (h *Hub) Notify(ctx context.Context, chainID, origin string, dataTypes []int32) {
	e := event{Chain: chainID, Origin: origin, DataTypes: dataTypes}
	if h.pool == nil {
		h.dispatch(e)
		return
	}
	payload, err := json.Marshal(e)
	if err != nil {
		h.log.Error("encode sync event", "error", err)
		return
	}
	if _, err := h.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, string(payload)); err != nil {
		// Devices still get the change on their next poll.
		h.log.Error("publish sync event", "error", err)
	}
}

// Listen receives events from Postgres until ctx ends. When the connection
// breaks it reconnects and has every device resync, since events may have
// been lost in between.
func (h *Hub) Listen(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		h.log.Error("sync event listener stopped, reconnecting", "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(retryInterval):
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	pooled, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The connection stays in LISTEN mode, so it does not go back to the pool.
	conn := pooled.Hijack()
	defer conn.Close(context.WithoutCancel(ctx)) //nolint:errcheck // the connection may be broken already
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	h.resyncAll()
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var e event
		if err := json.Unmarshal([]byte(notification.Payload), &e); err != nil {
			h.log.Error("decode sync event", "error", err)
			continue
		}
		h.dispatch(e)
	}
}

// subscriber is one open stream. Events wait in pending until the stream
// writes them, so a slow device gets them merged instead of losing some.
type subscriber struct {
	cacheGUID string
	wake      chan struct{}

	mu      sync.Mutex
	pending []int32
	resync  bool
}

func (s *subscriber) add(dataTypes []int32, resync bool) {
	s.mu.Lock()
	for _, dataType := range dataTypes {
		if !slices.Contains(s.pending, dataType) {
			s.pending = append(s.pending, dataType)
		}
	}
	s.resync = s.resync || resync
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscriber) take() (dataTypes []int32, resync bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dataTypes, resync = s.pending, s.resync
	s.pending, s.resync = nil, false
	return dataTypes, resync
}

func (h *Hub) dispatch(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subscribers[e.Chain] {
		if e.Origin == "" || sub.cacheGUID != e.Origin {
			sub.add(e.DataTypes, false)
		}
	}
}

func (h *Hub) resyncAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, subs := range h.subscribers {
		for sub := range subs {
			sub.add(nil, true)
		}
	}
}

func (h *Hub) subscribe(chainID, cacheGUID string) (*subscriber, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subscribers[chainID]
	if len(subs) >= h.maxPerChain || h.total >= h.maxTotal {
		return nil, false
	}
	if subs == nil {
		subs = map[*subscriber]struct{}{}
		h.subscribers[chainID] = subs
	}
	sub := &subscriber{cacheGUID: cacheGUID, wake: make(chan struct{}, 1)}
	subs[sub] = struct{}{}
	h.total++
	return sub, true
}

func (h *Hub) unsubscribe(chainID string, sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subscribers[chainID], sub)
	h.total--
	if len(h.subscribers[chainID]) == 0 {
		delete(h.subscribers, chainID)
	}
}

// ServeHTTP streams the events of the request's chain:
//
//	event: ready      the stream is open; fetch updates you may have missed
//	event: changed    data: {"types":[32904]}; fetch these data types
//	event: resync     events were lost; fetch all data types
//
// The device names itself with ?cache_guid= so it is not told about its own
// commits. A comment line every 25 seconds keeps proxies from closing the
// stream. It needs auth.Middleware in front.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	chainID := auth.ChainFromContext(r.Context())
	sub, ok := h.subscribe(chainID, r.URL.Query().Get("cache_guid"))
	if !ok {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many event streams", http.StatusTooManyRequests)
		return
	}
	defer h.unsubscribe(chainID, sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	write := func(format string, args ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write("retry: %d\nevent: ready\ndata: {}\n\n", retryInterval.Milliseconds()) {
		return
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		var sent bool
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			sent = write(": ping\n\n")
		case <-sub.wake:
			dataTypes, resync := sub.take()
			switch {
			case resync:
				sent = write("event: resync\ndata: {}\n\n")
			case len(dataTypes) > 0:
				data, _ := json.Marshal(map[string][]int32{"types": dataTypes})
				sent = write("event: changed\ndata: %s\n\n", data)
			default:
				sent = true
			}
		}
		if !sent {
			return
		}
	}
}
