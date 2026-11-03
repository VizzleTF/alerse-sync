package protocol

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

type entryResponse = pb.CommitResponse_EntryResponse

// commit stores the entities of a commit in one transaction and answers each
// one. An entity whose version differs from the stored one is a conflict:
// the client fetches the newer one and resolves it.
func (s *Service) commit(ctx context.Context, chainID string, msg *pb.ClientToServerMessage, rsp *pb.ClientToServerResponse) error {
	request := msg.GetCommit()
	now := s.now().UnixMilli()
	var responses []*entryResponse
	var written []int32

	err := s.store.Update(ctx, chainID, false, func(tx store.Tx) error {
		if err := checkBirthday(msg, tx.Chain(), false); err != nil {
			return err
		}
		// Commits may create a folder and its children together: children
		// name the folder by the ID the client gave it.
		serverIDs := map[string]string{}
		for _, entry := range request.GetEntries() {
			response, dataType, err := s.commitEntry(ctx, tx, request.GetCacheGuid(), entry, serverIDs, now)
			if err != nil {
				return err
			}
			responses = append(responses, response)
			if response.GetResponseType() == pb.CommitResponse_SUCCESS && dataType != 0 &&
				!slices.Contains(written, dataType) {
				written = append(written, dataType)
			}
		}
		return nil
	})
	if errors.Is(err, store.ErrNoChain) {
		return errBirthday
	}
	if err != nil {
		return err
	}
	rsp.Commit = &pb.CommitResponse{Entryresponse: responses}
	rsp.StoreBirthday = proto.String(msg.GetStoreBirthday())
	s.notify(ctx, chainID, request.GetCacheGuid(), written)
	return nil
}

// commitEntry stores one entity and returns the answer and the entity's data
// type. An error means the transaction has to fail.
func (s *Service) commitEntry(ctx context.Context, tx store.Tx, cacheGUID string, entry *pb.SyncEntity,
	serverIDs map[string]string, now int64,
) (*entryResponse, int32, error) {
	dataType := dataTypeOf(entry.GetSpecifics())
	if dataType == 0 {
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "entity has no data type"), 0, nil
	}
	if size := proto.Size(entry); size > s.limits.MaxEntityBytes {
		return rejected(pb.CommitResponse_INVALID_MESSAGE,
			fmt.Sprintf("entity is %d bytes, the limit is %d", size, s.limits.MaxEntityBytes)), 0, nil
	}

	if commitOnlyTypes[dataType] {
		return succeeded(entry.GetIdString(), 1), 0, nil
	}

	// Entities with a client tag have one ID per tag, so two devices that
	// create the same entity conflict instead of making two.
	var id string
	switch {
	case entry.GetClientTagHash() != "":
		id = fmt.Sprintf("%d.%s", dataType, entry.GetClientTagHash())
	case entry.GetVersion() > 0:
		id = entry.GetIdString()
	default:
		id = rand.Text()
	}
	existing, err := tx.Get(ctx, id)
	if err != nil {
		return nil, 0, err
	}

	switch {
	case existing == nil && entry.GetServerDefinedUniqueTag() != "":
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "clients cannot create permanent entities"), 0, nil
	case existing == nil && entry.GetDeleted():
		// Nothing to delete; other devices never had it.
		return succeeded(id, tx.NextVersion()), dataType, nil
	case existing == nil:
	case existing.DataType != dataType:
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "entity changed its data type"), 0, nil
	case existing.ServerTag != "" && entry.GetDeleted():
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "permanent entities cannot be deleted"), 0, nil
	case existing.Version != entry.GetVersion() && (!existing.Deleted || entry.GetVersion() != 0):
		// Only a deleted entity may be created again, from version 0, with
		// its client tag.
		return rejected(pb.CommitResponse_CONFLICT, "the server has a newer version"), 0, nil
	}

	revives := existing == nil || existing.Deleted
	if !entry.GetDeleted() && revives && tx.Chain().LiveEntities >= s.limits.MaxEntities {
		return rejected(pb.CommitResponse_OVER_QUOTA,
			fmt.Sprintf("the sync chain holds the maximum of %d entities", s.limits.MaxEntities)), 0, nil
	}

	stored, err := storedEntity(entry, existing, dataType, cacheGUID, now)
	if err != nil {
		return nil, 0, err
	}
	stored.IdString = proto.String(id)
	stored.Version = proto.Int64(tx.NextVersion())
	if parent, ok := serverIDs[stored.GetParentIdString()]; ok {
		stored.ParentIdString = proto.String(parent)
	}
	data, err := proto.Marshal(stored)
	if err != nil {
		return nil, 0, err
	}
	err = tx.Put(ctx, store.Entity{
		ID:        id,
		DataType:  dataType,
		Version:   stored.GetVersion(),
		Deleted:   stored.GetDeleted(),
		ServerTag: stored.GetServerDefinedUniqueTag(),
		Data:      data,
	})
	if err != nil {
		return nil, 0, err
	}
	serverIDs[entry.GetIdString()] = id
	return succeeded(id, stored.GetVersion()), dataType, nil
}

// storedEntity returns the entity to store for a commit of entry over
// existing, without its ID and version.
func storedEntity(entry *pb.SyncEntity, existing *store.Entity, dataType int32, cacheGUID string, now int64) (*pb.SyncEntity, error) {
	stored := proto.CloneOf(entry)
	// Only the server tags entities; permanent ones keep theirs below.
	stored.ServerDefinedUniqueTag = nil
	if stored.GetMtime() == 0 {
		stored.Mtime = proto.Int64(now)
	}
	if stored.GetDeleted() {
		stored.Specifics = emptySpecifics(dataType)
	}

	if existing == nil {
		if stored.GetCtime() == 0 {
			stored.Ctime = proto.Int64(now)
		}
		// Lets the creating device recognize the entity in its updates.
		if stored.GetOriginatorCacheGuid() == "" {
			stored.OriginatorCacheGuid = proto.String(cacheGUID)
			stored.OriginatorClientItemId = proto.String(entry.GetIdString())
		}
		return stored, nil
	}

	previous := &pb.SyncEntity{}
	if err := proto.Unmarshal(existing.Data, previous); err != nil {
		return nil, fmt.Errorf("entity %s: %w", existing.ID, err)
	}
	if stored.GetCtime() == 0 {
		stored.Ctime = previous.Ctime
	}
	if stored.GetOriginatorCacheGuid() == "" {
		stored.OriginatorCacheGuid = previous.OriginatorCacheGuid
		stored.OriginatorClientItemId = previous.OriginatorClientItemId
	}
	if existing.ServerTag != "" {
		// Clients update permanent entities, such as the Nigori node, but
		// do not move or rename them.
		stored.ServerDefinedUniqueTag = previous.ServerDefinedUniqueTag
		stored.ParentIdString = previous.ParentIdString
		stored.Name = previous.Name
		stored.NonUniqueName = previous.NonUniqueName
		stored.Folder = previous.Folder
	}
	return stored, nil
}

func succeeded(id string, version int64) *entryResponse {
	return &entryResponse{
		ResponseType: pb.CommitResponse_SUCCESS.Enum(),
		IdString:     proto.String(id),
		Version:      proto.Int64(version),
	}
}

func rejected(responseType pb.CommitResponse_ResponseType, reason string) *entryResponse {
	return &entryResponse{ResponseType: responseType.Enum(), ErrorMessage: proto.String(reason)}
}
