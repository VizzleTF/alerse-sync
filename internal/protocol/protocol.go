// Package protocol answers Chromium sync protocol requests
// (ClientToServerMessage) for one sync chain.
//
// It implements the subset of the protocol Chromium's sync engine uses:
// GetUpdates, Commit and ClearServerData. Entities are stored as the client
// sent them, with the server's ID, version and times, so data types newer
// than the server's protocol definitions sync too.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// Limits bounds what one chain may store and how clients talk to the server.
// A limit of 0 means no limit.
type Limits struct {
	// MaxEntities is the number of live entities a chain may hold of the
	// data types not counted below: bookmarks, passwords, settings and so on.
	MaxEntities int64
	// MaxBytes is the size of all entities a chain may store, tombstones
	// included.
	MaxBytes int64
	// MaxHistoryEntities and MaxHistoryBytes bound browsing history. A
	// commit beyond them removes the oldest visits instead of failing, as
	// they would expire anyway.
	MaxHistoryEntities int64
	MaxHistoryBytes    int64
	// MaxSessionEntities bounds the open tabs of the chain's devices.
	MaxSessionEntities int64
	// MaxEntityBytes is the largest entity a client may commit.
	MaxEntityBytes int
	// MaxUpdates and MaxUpdateBytes bound one GetUpdates response.
	MaxUpdates     int
	MaxUpdateBytes int
	// MaxChains is the number of chains the server keeps.
	MaxChains int64
	// PollInterval is how often clients poll when they get no events.
	PollInterval time.Duration
	// CommitBatchSize is the number of entities a client puts in a commit.
	CommitBatchSize int
	// SessionsNudgeDelay is how long a client waits before it commits a
	// change to its open tabs; 0 leaves Chromium's 15 seconds.
	SessionsNudgeDelay time.Duration
	// RequireCustomPassphrase refuses data a client could encrypt until the
	// chain's Nigori node says it is encrypted with a custom passphrase, and
	// unencrypted data of such types after that.
	RequireCustomPassphrase bool
}

// DefaultLimits suit a server for a few thousand chains.
var DefaultLimits = Limits{
	MaxEntities:        250_000,
	MaxBytes:           256 << 20,
	MaxHistoryEntities: 100_000,
	MaxHistoryBytes:    64 << 20,
	MaxSessionEntities: 20_000,
	MaxEntityBytes:     256 << 10,
	MaxUpdates:         500,
	MaxUpdateBytes:     16 << 20,
	PollInterval:       time.Hour,
	CommitBatchSize:    100,
	SessionsNudgeDelay: 3 * time.Second,
}

// Notifier tells a chain's other devices which data types changed.
type Notifier interface {
	Notify(ctx context.Context, chainID, cacheGUID string, dataTypes []int32)
}

// Service handles requests. It is safe for concurrent use.
type Service struct {
	store    store.Store
	notifier Notifier
	limits   Limits
	now      func() time.Time
	options
}

type options struct {
	allowCreate func(context.Context) (bool, time.Duration)
	evicted     func(int64)
}

// Option changes how a Service works.
type Option func(*options)

// WithCreateGate has the service ask allow before a request creates a chain.
// When it refuses, the client is throttled for the duration it returns.
func WithCreateGate(allow func(ctx context.Context) (bool, time.Duration)) Option {
	return func(o *options) { o.allowCreate = allow }
}

// WithEvictionObserver tells observe how many history entities a commit
// removed to stay within the limits.
func WithEvictionObserver(observe func(n int64)) Option {
	return func(o *options) { o.evicted = observe }
}

// New returns a service. notifier may be nil.
func New(st store.Store, notifier Notifier, limits Limits, opts ...Option) *Service {
	s := &Service{store: st, notifier: notifier, limits: limits, now: time.Now}
	for _, opt := range opts {
		opt(&s.options)
	}
	return s
}

var (
	// errBirthday ends a request whose store birthday does not match the
	// chain.
	errBirthday = errors.New("store birthday does not match")
	// errFull ends the first request of a new chain when the server keeps
	// MaxChains chains.
	errFull = errors.New("the server has no room for another sync chain")
)

// throttled ends a request that has to wait before it may create a chain.
type throttled struct{ wait time.Duration }

func (throttled) Error() string { return "too many new sync chains" }

// Handle answers one request of chain chainID. Problems with the request are
// reported in the response; an error means the server failed.
func (s *Service) Handle(ctx context.Context, chainID string, msg *pb.ClientToServerMessage) (*pb.ClientToServerResponse, error) {
	rsp := &pb.ClientToServerResponse{
		ErrorCode: pb.SyncEnums_SUCCESS.Enum(),
		ClientCommand: &pb.ClientCommand{
			SetSyncPollInterval: proto.Int32(int32(s.limits.PollInterval / time.Second)),
			MaxCommitBatchSize:  proto.Int32(int32(s.limits.CommitBatchSize)),
		},
	}
	if d := s.limits.SessionsNudgeDelay; d > 0 {
		// Open tabs reach the other devices in seconds: Chromium waits 15 s
		// to merge a page load's commits, and applies the server's delay as is.
		rsp.ClientCommand.CustomNudgeDelays = []*pb.CustomNudgeDelay{{
			DatatypeId: proto.Int32(sessionType),
			DelayMs:    proto.Int32(int32(d / time.Millisecond)),
		}}
	}

	var err error
	var wait throttled
	switch msg.GetMessageContents() {
	case pb.ClientToServerMessage_GET_UPDATES:
		err = s.getUpdates(ctx, chainID, msg, rsp)
	case pb.ClientToServerMessage_COMMIT:
		err = s.commit(ctx, chainID, msg, rsp)
	case pb.ClientToServerMessage_CLEAR_SERVER_DATA:
		err = s.clear(ctx, chainID, msg, rsp)
	default:
		return failure(pb.SyncEnums_UNKNOWN,
			fmt.Sprintf("unsupported message contents %v", msg.GetMessageContents())), nil
	}
	switch {
	case errors.Is(err, errBirthday):
		// Chromium turns sync off and clears its data when the birthday in
		// the response differs from its own, or when there is none.
		rsp = failure(pb.SyncEnums_NOT_MY_BIRTHDAY, "the sync chain was reset; start over")
		chain, ok, err := s.store.Chain(ctx, chainID)
		if err != nil {
			return nil, err
		}
		if ok {
			rsp.StoreBirthday = proto.String(chain.Birthday)
		}
		return rsp, nil
	case errors.Is(err, errFull):
		return failure(pb.SyncEnums_TRANSIENT_ERROR, err.Error()), nil
	case errors.As(err, &wait):
		return Throttled(wait.wait), nil
	case err != nil:
		return nil, err
	}
	return rsp, nil
}

// checkBirthday accepts a request that names the chain's birthday. Only a
// client's first GetUpdates, which learns the birthday, may leave it out.
func checkBirthday(msg *pb.ClientToServerMessage, chain store.Chain, allowEmpty bool) error {
	birthday := msg.GetStoreBirthday()
	if birthday == chain.Birthday || (birthday == "" && allowEmpty) {
		return nil
	}
	return errBirthday
}

func (s *Service) clear(ctx context.Context, chainID string, msg *pb.ClientToServerMessage, rsp *pb.ClientToServerResponse) error {
	chain, ok, err := s.store.Chain(ctx, chainID)
	if err != nil {
		return err
	}
	if ok {
		if err := checkBirthday(msg, chain, true); err != nil {
			return err
		}
		// Deleting by birthday leaves a chain created again meanwhile alone.
		if err := s.store.Delete(ctx, chainID, chain.Birthday); err != nil {
			return err
		}
		// Other devices fetch updates, get NOT_MY_BIRTHDAY and start over.
		s.notify(ctx, chainID, "", []int32{nigoriType})
	}
	rsp.ClearServerData = &pb.ClearServerDataResponse{}
	return nil
}

func (s *Service) notify(ctx context.Context, chainID, cacheGUID string, dataTypes []int32) {
	if s.notifier != nil && len(dataTypes) > 0 {
		s.notifier.Notify(ctx, chainID, cacheGUID, dataTypes)
	}
}

// failure reports an error by error_code alone, from which Chromium derives
// what to do. An explicit error field would replace that, and the protocol
// no longer has an action for a reset chain.
func failure(code pb.SyncEnums_ErrorType, description string) *pb.ClientToServerResponse {
	return &pb.ClientToServerResponse{ErrorCode: code.Enum(), ErrorMessage: proto.String(description)}
}

// Throttled is the response to a client that sends requests too fast.
func Throttled(retryAfter time.Duration) *pb.ClientToServerResponse {
	rsp := failure(pb.SyncEnums_THROTTLED, "too many requests")
	rsp.Error = &pb.ClientToServerResponse_Error{ErrorType: pb.SyncEnums_THROTTLED.Enum()}
	rsp.ClientCommand = &pb.ClientCommand{
		ThrottleDelaySeconds: proto.Int32(int32(max(retryAfter, time.Second) / time.Second)),
	}
	return rsp
}
