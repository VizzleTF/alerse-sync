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
	bookmarkType               = 32904
	nigoriType                 = 47745
	sessionType                = 50119
	historyDeleteDirectiveType = 150251
	historyType                = 963985
)

// ExpiringTypes are the data types whose entities the server removes some
// time after they were last written, as Google's server does: Chromium keeps
// history for 90 days and sends no deletion when a visit expires, leaving
// that to the server. Delete directives older than the history they delete
// have nothing left to delete.
var ExpiringTypes = []int32{historyType, historyDeleteDirectiveType}

// quotaGroup is a set of data types that share a limit.
type quotaGroup int

const (
	// otherGroup is everything below MaxEntities: bookmarks, passwords,
	// settings and the rest.
	otherGroup quotaGroup = iota
	historyGroup
	sessionGroup
)

func groupOf(dataType int32) quotaGroup {
	switch dataType {
	case historyType, historyDeleteDirectiveType:
		return historyGroup
	case sessionType:
		return sessionGroup
	}
	return otherGroup
}

// encryptableTypes are the data types Chromium encrypts when the chain has
// a custom passphrase (EncryptableUserTypes in components/sync/base/
// data_type.cc). The others, like device info and the Nigori node itself,
// are never encrypted.
var encryptableTypes = map[int32]bool{
	31729: true, 32904: true, 37702: true, 41210: true, 45873: true, 48119: true, 48364: true,
	50119: true, 63951: true, 88610: true, 96159: true, 103656: true, 150251: true, 170540: true,
	229170: true, 330441: true, 340906: true, 410745: true, 411028: true, 601980: true, 662827: true,
	673225: true, 702141: true, 874841: true, 895275: true, 963985: true, 974304: true, 1004874: true,
	1164238: true, 1281100: true, 1329438: true, 1429255: true, 1517585: true, 1518931: true,
	1569348: true, 1577986: true, 1587331: true, 1656053: true,
}

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

// EmptyChainVersion is the version of a chain no device committed to: it
// holds only the permanent entities.
var EmptyChainVersion = int64(len(permanentEntities))

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
// They are opaque to clients. An incremental token is a format byte and the
// version. A client that downloads all entities of a type, the first time or
// after the server removed tombstones it may not have seen, gets snapshot
// tokens until it has them: the version it got to and the chain's version
// when the download started. Tombstones are left out of that download.
const (
	tokenIncremental = 1
	tokenSnapshot    = 2
)

type token struct {
	// after is the version the client has.
	after int64
	// until is set while the client downloads the entities up to it.
	until int64
}

func encodeToken(version int64) []byte {
	return binary.BigEndian.AppendUint64([]byte{tokenIncremental}, uint64(version))
}

func encodeSnapshotToken(after, until int64) []byte {
	token := binary.BigEndian.AppendUint64([]byte{tokenSnapshot}, uint64(after))
	return binary.BigEndian.AppendUint64(token, uint64(until))
}

// decodeToken reads a token. An empty or unknown token means the client has
// nothing yet.
func decodeToken(raw []byte) token {
	switch {
	case len(raw) == 9 && raw[0] == tokenIncremental:
		return token{after: max(int64(binary.BigEndian.Uint64(raw[1:])), 0)}
	case len(raw) == 17 && raw[0] == tokenSnapshot:
		t := token{
			after: max(int64(binary.BigEndian.Uint64(raw[1:9])), 0),
			until: max(int64(binary.BigEndian.Uint64(raw[9:])), 0),
		}
		if t.until > 0 && t.after <= t.until {
			return t
		}
	}
	return token{}
}
