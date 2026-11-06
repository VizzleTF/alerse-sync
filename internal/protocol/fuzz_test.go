package protocol

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

func FuzzDataTypeOf(f *testing.F) {
	for _, dataType := range []int32{bookmarkType, historyType, 9_999_999} {
		data, _ := proto.Marshal(emptySpecifics(dataType))
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		specifics := &pb.EntitySpecifics{}
		if proto.Unmarshal(data, specifics) != nil {
			return
		}
		if dataType := dataTypeOf(specifics); dataType < 0 {
			t.Fatalf("data type %d", dataType)
		}
	})
}

func FuzzDecodeToken(f *testing.F) {
	f.Add(encodeToken(5))
	f.Add(encodeSnapshotToken(3, 9))
	f.Fuzz(func(t *testing.T, raw []byte) {
		tok := decodeToken(raw)
		if tok.after < 0 || tok.until < 0 || (tok.until > 0 && tok.after > tok.until) {
			t.Fatalf("token %+v", tok)
		}
	})
}

// FuzzHandle sends arbitrary requests to a chain with some data: the service
// may refuse them but must not fail or break the chain.
func FuzzHandle(f *testing.F) {
	for _, msg := range []*pb.ClientToServerMessage{
		{MessageContents: pb.ClientToServerMessage_GET_UPDATES.Enum(), GetUpdates: &pb.GetUpdatesMessage{
			FromProgressMarker: []*pb.DataTypeProgressMarker{{DataTypeId: proto.Int32(bookmarkType), Token: encodeSnapshotToken(1, 3)}},
		}},
		{MessageContents: pb.ClientToServerMessage_COMMIT.Enum(), Commit: &pb.CommitMessage{
			Entries: []*pb.SyncEntity{bookmark("c1", "bookmark_bar", "x"), preference("p", "v")},
		}},
	} {
		data, _ := proto.Marshal(msg)
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		msg := &pb.ClientToServerMessage{}
		if proto.Unmarshal(data, msg) != nil {
			return
		}
		s := New(memory.New(), nil, DefaultLimits)
		d := newDevice(t, s, "a")
		d.updates(bookmarkType)
		msg.StoreBirthday = proto.String(d.birthday)
		if _, err := s.Handle(context.Background(), chainID, msg); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		// A new device syncs everything, even if the request cleared the
		// chain.
		newDevice(t, s, "b").updates(bookmarkType, preferenceType, nigoriType)
	})
}
