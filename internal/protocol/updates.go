package protocol

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// maxCountRemaining bounds the count behind changes_remaining, which clients
// only compare with 0.
const maxCountRemaining = 10_000

// getUpdates returns, for each data type the client asks about, the entities
// that changed after the client's progress token, oldest first. A response
// holds at most MaxUpdates entities and about MaxUpdateBytes;
// changes_remaining tells the client to ask again.
//
// A client that has nothing of a data type yet gets its live entities
// without tombstones. So does a client whose token is older than a tombstone
// the server removed, which may have missed a deletion: its first response
// carries a garbage collection directive, so it drops what the download does
// not have.
func (s *Service) getUpdates(ctx context.Context, chainID string, msg *pb.ClientToServerMessage, rsp *pb.ClientToServerResponse) error {
	chain, err := s.chain(ctx, chainID)
	if err != nil {
		return err
	}
	if err := checkBirthday(msg, chain, true); err != nil {
		return err
	}
	if s.now().Sub(chain.LastSeen) >= store.TouchInterval {
		// Only marks the chain as in use; a failure changes nothing else.
		_ = s.store.Touch(ctx, chainID)
	}
	request := msg.GetGetUpdates()
	u := &updates{
		service: s, chain: chain,
		rsp: &pb.GetUpdatesResponse{ChangesRemaining: proto.Int64(0)},
	}
	seen := map[int32]bool{}
	for _, marker := range request.GetFromProgressMarker() {
		dataType := marker.GetDataTypeId()
		if dataType == 0 || seen[dataType] {
			continue
		}
		seen[dataType] = true
		if err := u.add(ctx, dataType, decodeToken(marker.GetToken())); err != nil {
			return err
		}
	}

	if request.GetNeedEncryptionKey() {
		u.rsp.EncryptionKeys = [][]byte{chain.KeystoreKey}
	}
	rsp.GetUpdates = u.rsp
	rsp.StoreBirthday = proto.String(chain.Birthday)
	return nil
}

// updates builds a GetUpdates response.
type updates struct {
	service *Service
	chain   store.Chain
	rsp     *pb.GetUpdatesResponse
	size    int
}

// add adds the changes of a data type after tok to the response.
func (u *updates) add(ctx context.Context, dataType int32, tok token) error {
	state := u.chain.Types[dataType]
	marker := &pb.DataTypeProgressMarker{DataTypeId: proto.Int32(dataType)}
	u.rsp.NewProgressMarker = append(u.rsp.NewProgressMarker, marker)

	if tok.until == 0 {
		switch {
		case tok.after > 0 && state.PurgedVersion > tok.after:
			// The client may have missed the deletion of a removed
			// tombstone: it starts over, dropping what it does not get.
			marker.GcDirective = &pb.GarbageCollectionDirective{VersionWatermark: proto.Int64(state.PurgedVersion)}
			tok = token{until: u.chain.Version}
		case tok.after == 0:
			tok = token{until: u.chain.Version}
		case tok.after >= state.Version:
			// Nothing changed. Versions up to the chain's are settled,
			// so the next request can skip the type without looking.
			marker.Token = encodeToken(max(tok.after, u.chain.Version))
			return nil
		}
	}
	if tok.until > 0 && state.Entities == 0 {
		// A download of nothing.
		tok = token{after: tok.until}
		if state.Version <= tok.after {
			marker.Token = encodeToken(max(tok.after, u.chain.Version))
			return nil
		}
	}

	change := store.Change{DataType: dataType, After: tok.after, Until: tok.until, LiveOnly: tok.until > 0}
	entities, complete, err := u.fetch(ctx, change)
	if err != nil {
		return err
	}
	version := tok.after
	if len(entities) > 0 {
		version = entities[len(entities)-1].Version
	}

	switch {
	case !complete:
		left, err := u.service.store.CountSince(ctx, u.chain.ID,
			store.Change{DataType: dataType, After: version, Until: tok.until, LiveOnly: change.LiveOnly},
			maxCountRemaining)
		if err != nil {
			return err
		}
		*u.rsp.ChangesRemaining += max(left, 1)
		if tok.until > 0 {
			marker.Token = encodeSnapshotToken(version, tok.until)
		} else {
			marker.Token = encodeToken(version)
		}
	case tok.until > 0:
		// The download is done; what changed since it began follows.
		marker.Token = encodeToken(tok.until)
		if state.Version > tok.until {
			*u.rsp.ChangesRemaining++
		}
	default:
		// Every change up to the chain's version, read before the changes,
		// is in the response.
		marker.Token = encodeToken(max(version, u.chain.Version))
	}
	return nil
}

// fetch adds the entities change selects to the response as far as they fit
// and reports whether all of them did.
func (u *updates) fetch(ctx context.Context, change store.Change) ([]store.Entity, bool, error) {
	limits := u.service.limits
	room := limits.MaxUpdates - len(u.rsp.Entries)
	if room <= 0 {
		return nil, false, nil
	}
	// One more than fits tells whether the client has to come back.
	entities, err := u.service.store.ChangesSince(ctx, u.chain.ID, change, room+1)
	if err != nil {
		return nil, false, err
	}
	complete := len(entities) <= room
	entities = entities[:min(room, len(entities))]
	for i, e := range entities {
		// The first entity always fits, so a large one cannot block.
		if u.size += len(e.Data); u.size > limits.MaxUpdateBytes && len(u.rsp.Entries) > 0 {
			return entities[:i], false, nil
		}
		entity := &pb.SyncEntity{}
		if err := proto.Unmarshal(e.Data, entity); err != nil {
			return nil, false, fmt.Errorf("entity %s of chain %s: %w", e.ID, u.chain.ID, err)
		}
		u.rsp.Entries = append(u.rsp.Entries, entity)
	}
	return entities, complete, nil
}

// chain returns the chain, creating it with its permanent entities on the
// first request of its first device.
func (s *Service) chain(ctx context.Context, chainID string) (store.Chain, error) {
	chain, ok, err := s.store.Chain(ctx, chainID)
	if err != nil || ok {
		return chain, err
	}
	if s.allowCreate != nil {
		if ok, wait := s.allowCreate(ctx); !ok {
			return chain, throttled{wait}
		}
	}
	err = s.store.Create(ctx, chainID, s.limits.MaxChains, func(tx store.Tx) error {
		if tx.Created() {
			if err := createPermanentEntities(ctx, tx, s.now().UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, store.ErrFull) {
		return chain, errFull
	}
	if err != nil {
		return chain, err
	}
	// Read again for the state as committed, with its data types.
	chain, _, err = s.store.Chain(ctx, chainID)
	return chain, err
}
