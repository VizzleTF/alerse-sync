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
	entries := request.GetEntries()
	var c *committer

	err := s.store.Update(ctx, chainID, func(tx store.Tx) error {
		if err := checkBirthday(msg, tx.Chain(), false); err != nil {
			return err
		}
		c = &committer{
			service: s, tx: tx, cacheGUID: request.GetCacheGuid(), now: s.now().UnixMilli(),
			serverIDs: map[string]string{}, responses: make([]*entryResponse, len(entries)),
		}
		return c.commit(ctx, entries)
	})
	if errors.Is(err, store.ErrNoChain) {
		return errBirthday
	}
	if err != nil {
		return err
	}
	if c.evicted > 0 && s.evicted != nil {
		s.evicted(c.evicted)
	}
	rsp.Commit = &pb.CommitResponse{Entryresponse: c.responses}
	rsp.StoreBirthday = proto.String(msg.GetStoreBirthday())
	s.notify(ctx, chainID, request.GetCacheGuid(), c.written)
	return nil
}

// committer is one commit's work in its transaction.
type committer struct {
	service   *Service
	tx        store.Tx
	cacheGUID string
	now       int64
	// serverIDs maps the IDs a client gave new entities to theirs: commits
	// may create a folder and its children together, and children name the
	// folder by the client's ID.
	serverIDs map[string]string
	responses []*entryResponse
	written   []int32
	evicted   int64
}

func (c *committer) commit(ctx context.Context, entries []*pb.SyncEntity) error {
	// One read for all the stored entities the commit names.
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if id, ok := entityID(entry, dataTypeOf(entry.GetSpecifics())); ok {
			ids = append(ids, id)
		}
	}
	if err := c.tx.Prefetch(ctx, ids); err != nil {
		return err
	}

	for i := range entries {
		response, dataType, err := c.entry(ctx, entries[i])
		if err != nil {
			return err
		}
		c.responses[i] = response
		if response.GetResponseType() == pb.CommitResponse_SUCCESS && dataType != 0 &&
			!slices.Contains(c.written, dataType) {
			c.written = append(c.written, dataType)
		}
	}
	return nil
}

// entityID returns the server's ID of a committed entity, or false if it is
// a new one without a client tag, which gets a new ID.
func entityID(entry *pb.SyncEntity, dataType int32) (string, bool) {
	// Entities with a client tag have one ID per tag, so two devices that
	// create the same entity conflict instead of making two.
	switch {
	case entry.GetClientTagHash() != "":
		return fmt.Sprintf("%d.%s", dataType, entry.GetClientTagHash()), true
	case entry.GetVersion() > 0:
		return entry.GetIdString(), true
	}
	return "", false
}

// entry stores one entity and returns the answer and the entity's data type.
// An error means the transaction has to fail.
func (c *committer) entry(ctx context.Context, entry *pb.SyncEntity) (*entryResponse, int32, error) {
	limits := c.service.limits
	dataType := dataTypeOf(entry.GetSpecifics())
	if dataType == 0 {
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "entity has no data type"), 0, nil
	}
	if size := proto.Size(entry); size > limits.MaxEntityBytes {
		return rejected(pb.CommitResponse_INVALID_MESSAGE,
			fmt.Sprintf("entity is %d bytes, the limit is %d", size, limits.MaxEntityBytes)), 0, nil
	}

	if commitOnlyTypes[dataType] {
		return succeeded(entry.GetIdString(), 1), 0, nil
	}
	id, ok := entityID(entry, dataType)
	if !ok {
		id = rand.Text()
	}
	existing, err := c.tx.Get(ctx, id)
	if err != nil {
		return nil, 0, err
	}

	switch {
	case existing == nil && entry.GetServerDefinedUniqueTag() != "":
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "clients cannot create permanent entities"), 0, nil
	case existing == nil && entry.GetDeleted():
		// Nothing to delete; other devices never had it.
		return succeeded(id, c.tx.NextVersion()), dataType, nil
	case existing == nil:
	case existing.DataType != dataType:
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "entity changed its data type"), 0, nil
	case existing.ServerTag != "" && entry.GetDeleted():
		return rejected(pb.CommitResponse_INVALID_MESSAGE, "permanent entities cannot be deleted"), 0, nil
	case existing.Version != entry.GetVersion() && entry.GetVersion() == 0 && overwriteTypes[dataType]:
		// The client no longer knows the version; its commit replaces the
		// entity.
	case existing.Version != entry.GetVersion() && (!existing.Deleted || entry.GetVersion() != 0):
		// Only a deleted entity may be created again, from version 0, with
		// its client tag.
		return rejected(pb.CommitResponse_CONFLICT, "the server has a newer version"), 0, nil
	}

	stored, err := storedEntity(entry, existing, dataType, c.cacheGUID, c.now)
	if err != nil {
		return nil, 0, err
	}
	stored.IdString = proto.String(id)
	stored.Version = proto.Int64(c.tx.NextVersion())
	if parent, ok := c.serverIDs[stored.GetParentIdString()]; ok {
		stored.ParentIdString = proto.String(parent)
	}
	data, err := proto.Marshal(stored)
	if err != nil {
		return nil, 0, err
	}
	if !stored.GetDeleted() {
		if refusal, err := c.quota(ctx, dataType, existing, int64(len(data))); refusal != nil || err != nil {
			return refusal, 0, err
		}
	}
	err = c.tx.Put(ctx, store.Entity{
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
	c.serverIDs[entry.GetIdString()] = id
	return succeeded(id, stored.GetVersion()), dataType, nil
}

// quota checks that the chain has room to store size bytes of an entity of
// dataType over existing. History makes room by dropping its oldest visits,
// for itself and for the other data types. It returns the answer to a
// commit that does not fit.
func (c *committer) quota(ctx context.Context, dataType int32, existing *store.Entity, size int64) (*entryResponse, error) {
	limits := c.service.limits
	added := existing == nil || existing.Deleted
	growth := size
	if existing != nil {
		growth -= int64(len(existing.Data))
	}

	// check returns the answer if the entity does not fit, and how much
	// history would have to go for it to fit.
	check := func() (refusal *entryResponse, entities, bytes int64) {
		chain := c.tx.Chain()
		var history store.TypeState
		var sessions int64
		for dataType, state := range chain.Types {
			switch groupOf(dataType) {
			case historyGroup:
				history.Entities += state.Entities
				history.Bytes += state.Bytes
			case sessionGroup:
				sessions += state.Entities
			}
		}
		if growth > 0 && limits.MaxBytes > 0 && chain.Bytes+growth > limits.MaxBytes {
			bytes = chain.Bytes + growth - limits.MaxBytes
			refusal = rejected(pb.CommitResponse_OVER_QUOTA,
				fmt.Sprintf("the sync chain holds the maximum of %d bytes", limits.MaxBytes))
		}
		switch groupOf(dataType) {
		case historyGroup:
			if added && limits.MaxHistoryEntities > 0 && history.Entities >= limits.MaxHistoryEntities {
				entities = history.Entities + 1 - limits.MaxHistoryEntities
				refusal = rejected(pb.CommitResponse_OVER_QUOTA, "the sync chain holds the maximum of history")
			}
			if growth > 0 && limits.MaxHistoryBytes > 0 && history.Bytes+growth > limits.MaxHistoryBytes {
				bytes = max(bytes, history.Bytes+growth-limits.MaxHistoryBytes)
				refusal = rejected(pb.CommitResponse_OVER_QUOTA, "the sync chain holds the maximum of history")
			}
		case sessionGroup:
			if added && limits.MaxSessionEntities > 0 && sessions >= limits.MaxSessionEntities {
				return rejected(pb.CommitResponse_OVER_QUOTA, fmt.Sprintf(
					"the sync chain holds the maximum of %d open tabs and windows", limits.MaxSessionEntities)), 0, 0
			}
		default:
			if added && limits.MaxEntities > 0 && chain.LiveEntities-history.Entities-sessions >= limits.MaxEntities {
				return rejected(pb.CommitResponse_OVER_QUOTA,
					fmt.Sprintf("the sync chain holds the maximum of %d entities", limits.MaxEntities)), 0, 0
			}
		}
		return refusal, entities, bytes
	}

	refusal, entities, bytes := check()
	if refusal == nil || (entities == 0 && bytes == 0) {
		return refusal, nil
	}
	// Drop a hundredth more than needed, so that not every commit has to.
	if entities > 0 {
		entities += limits.MaxHistoryEntities / 100
	}
	if bytes > 0 {
		bytes += max(limits.MaxHistoryBytes, limits.MaxBytes) / 100
	}
	n, _, err := c.tx.Evict(ctx, historyType, entities, bytes)
	if err != nil {
		return nil, err
	}
	c.evicted += n
	refusal, _, _ = check()
	return refusal, nil
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
