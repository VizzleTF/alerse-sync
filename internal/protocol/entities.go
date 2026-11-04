package protocol

import (
	"context"
	"encoding/binary"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// Data type IDs are the field numbers of EntitySpecifics.specifics_variant.
const (
	bookmarkType = 32904
	nigoriType   = 47745
	historyType  = 963985
)

// commitOnlyTypes are data types clients send and never fetch (Chromium's
// CommitOnlyTypes): user events, user consents, security events, sharing
// messages, password sharing invitations and encrypted tab context items.
// Google's server passes them on; this server has nowhere to pass them, so
// it accepts and drops them.
var commitOnlyTypes = map[int32]bool{
	455206: true, 556014: true, 600372: true, 728866: true, 1142081: true, 1661069: true,
}

// overwriteTypes are data types whose clients forget an entity's version once
// it is committed: Chromium's history bridge stops tracking synced visits, so
// a later change to a visit comes from version 0. Google's server lets such a
// commit replace the entity; so does this one.
var overwriteTypes = map[int32]bool{
	historyType: true,
}

// rootParentID is the parent of the top-level permanent entities.
const rootParentID = "0"

// permanentEntities are created with every chain, as Google's server does:
// Chromium expects the bookmark folders and the Nigori node to exist. Their
// IDs are their tags.
var permanentEntities = []struct {
	tag, name, parent string
	dataType          int32
	folder            bool
}{
	{"google_chrome_bookmarks", "Bookmarks", rootParentID, bookmarkType, true},
	{"bookmark_bar", "Bookmark Bar", "google_chrome_bookmarks", bookmarkType, true},
	{"other_bookmarks", "Other Bookmarks", "google_chrome_bookmarks", bookmarkType, true},
	{"synced_bookmarks", "Synced Bookmarks", "google_chrome_bookmarks", bookmarkType, true},
	{"google_chrome_nigori", "Nigori", rootParentID, nigoriType, false},
}

func createPermanentEntities(ctx context.Context, tx store.Tx, now int64) error {
	for _, p := range permanentEntities {
		version := tx.NextVersion()
		entity := &pb.SyncEntity{
			IdString:               proto.String(p.tag),
			ParentIdString:         proto.String(p.parent),
			Version:                proto.Int64(version),
			Mtime:                  proto.Int64(now),
			Ctime:                  proto.Int64(now),
			Name:                   proto.String(p.name),
			NonUniqueName:          proto.String(p.name),
			ServerDefinedUniqueTag: proto.String(p.tag),
			Folder:                 proto.Bool(p.folder),
			Specifics:              emptySpecifics(p.dataType),
		}
		data, err := proto.Marshal(entity)
		if err != nil {
			return err
		}
		err = tx.Put(ctx, store.Entity{
			ID: p.tag, DataType: p.dataType, Version: version, ServerTag: p.tag, Data: data,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// dataTypeOf returns the data type of specifics, or 0 if none is set.
// Encrypted specifics still set their data type's field, empty.
func dataTypeOf(specifics *pb.EntitySpecifics) int32 {
	if specifics == nil {
		return 0
	}
	message := specifics.ProtoReflect()
	if field := message.WhichOneof(message.Descriptor().Oneofs().ByName("specifics_variant")); field != nil {
		return int32(field.Number())
	}
	// A data type newer than proto/ arrives as an unknown message field.
	unknown := message.GetUnknown()
	for len(unknown) > 0 {
		number, wireType, n := protowire.ConsumeTag(unknown)
		if n < 0 {
			return 0
		}
		if wireType == protowire.BytesType {
			return int32(number)
		}
		m := protowire.ConsumeFieldValue(number, wireType, unknown[n:])
		if m < 0 {
			return 0
		}
		unknown = unknown[n+m:]
	}
	return 0
}

// emptySpecifics returns specifics with only the data type set: the
// specifics of a permanent entity or a deleted one.
func emptySpecifics(dataType int32) *pb.EntitySpecifics {
	specifics := &pb.EntitySpecifics{}
	message := specifics.ProtoReflect()
	number := protoreflect.FieldNumber(dataType)
	if field := message.Descriptor().Fields().ByNumber(number); field != nil {
		message.Set(field, message.NewField(field))
	} else {
		message.SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), nil))
	}
	return specifics
}

// Progress tokens tell the server which version of a data type a client has.
// They are opaque to clients: a format byte and the version.
const tokenFormat = 1

func encodeToken(version int64) []byte {
	return binary.BigEndian.AppendUint64([]byte{tokenFormat}, uint64(version))
}

// decodeToken returns the version in token. An empty or unknown token means
// the client has nothing yet.
func decodeToken(token []byte) int64 {
	if len(token) != 9 || token[0] != tokenFormat {
		return 0
	}
	return max(int64(binary.BigEndian.Uint64(token[1:])), 0)
}
