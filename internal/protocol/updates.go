package protocol

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// getUpdates returns, for each data type the client asks about, the entities
// that changed after the client's progress token, oldest first. A response
// holds at most MaxUpdates entities and about MaxUpdateBytes;
// changes_remaining tells the client to ask again.
func (s *Service) getUpdates(ctx context.Context, chainID string, msg *pb.ClientToServerMessage, rsp *pb.ClientToServerResponse) error {
	chain, err := s.chain(ctx, chainID)
	if err != nil {
		return err
	}
	if err := checkBirthday(msg, chain, true); err != nil {
		return err
	}
	request := msg.GetGetUpdates()
	updates := &pb.GetUpdatesResponse{ChangesRemaining: proto.Int64(0)}
	seen := map[int32]bool{}
	size := 0

	for _, marker := range request.GetFromProgressMarker() {
		dataType := marker.GetDataTypeId()
		if dataType == 0 || seen[dataType] {
			continue
		}
		seen[dataType] = true
		change := store.Change{DataType: dataType, After: decodeToken(marker.GetToken())}

		room := s.limits.MaxUpdates - len(updates.Entries)
		var entities []store.Entity
		complete := false
		if room > 0 {
			// One more than fits tells whether the client has to come back.
			if entities, err = s.store.ChangesSince(ctx, chainID, change, room+1); err != nil {
				return err
			}
			complete = len(entities) <= room
			entities = entities[:min(room, len(entities))]
		}
		for i, e := range entities {
			// The first entity always fits, so a large one cannot block.
			if size += len(e.Data); size > s.limits.MaxUpdateBytes && len(updates.Entries) > 0 {
				entities, complete = entities[:i], false
				break
			}
			entity := &pb.SyncEntity{}
			if err := proto.Unmarshal(e.Data, entity); err != nil {
				return fmt.Errorf("entity %s of chain %s: %w", e.ID, chainID, err)
			}
			updates.Entries = append(updates.Entries, entity)
		}

		version := change.After
		if len(entities) > 0 {
			version = entities[len(entities)-1].Version
		}
		if !complete {
			left, err := s.store.CountSince(ctx, chainID, store.Change{DataType: dataType, After: version})
			if err != nil {
				return err
			}
			*updates.ChangesRemaining += left
		}
		updates.NewProgressMarker = append(updates.NewProgressMarker, &pb.DataTypeProgressMarker{
			DataTypeId: proto.Int32(dataType),
			Token:      encodeToken(version),
		})
	}

	if request.GetNeedEncryptionKey() {
		updates.EncryptionKeys = [][]byte{chain.KeystoreKey}
	}
	rsp.GetUpdates = updates
	rsp.StoreBirthday = proto.String(chain.Birthday)
	return nil
}

// chain returns the chain, creating it with its permanent entities on the
// first request of its first device.
func (s *Service) chain(ctx context.Context, chainID string) (store.Chain, error) {
	chain, ok, err := s.store.Chain(ctx, chainID)
	if err != nil || ok {
		return chain, err
	}
	if s.limits.MaxChains > 0 {
		count, err := s.store.CountChains(ctx)
		if err != nil {
			return chain, err
		}
		if count >= s.limits.MaxChains {
			return chain, errFull
		}
	}
	err = s.store.Update(ctx, chainID, true, func(tx store.Tx) error {
		if tx.Created() {
			if err := createPermanentEntities(ctx, tx, s.now().UnixMilli()); err != nil {
				return err
			}
		}
		chain = tx.Chain()
		return nil
	})
	return chain, err
}
