package protocol

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

func visit(tag, title string) *pb.SyncEntity {
	return &pb.SyncEntity{
		IdString:      proto.String("c-" + tag),
		ClientTagHash: proto.String(tag),
		Specifics: &pb.EntitySpecifics{SpecificsVariant: &pb.EntitySpecifics_History{
			History: &pb.HistorySpecifics{OriginatorCacheGuid: proto.String(title)},
		}},
	}
}

func tab(tag string) *pb.SyncEntity {
	return &pb.SyncEntity{
		IdString:      proto.String("c-" + tag),
		ClientTagHash: proto.String(tag),
		Specifics: &pb.EntitySpecifics{SpecificsVariant: &pb.EntitySpecifics_Session{
			Session: &pb.SessionSpecifics{SessionTag: proto.String(tag)},
		}},
	}
}

func chainState(t *testing.T, s *Service) store.Chain {
	t.Helper()
	chain, ok, err := s.store.Chain(context.Background(), chainID)
	if err != nil || !ok {
		t.Fatalf("chain: %v %v", ok, err)
	}
	return chain
}

func TestByteQuota(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxBytes = 2000 })
	a := newDevice(t, s, "a")
	a.updates(bookmarkType)

	big := a.commitOne(bookmark("c1", "bookmark_bar", strings.Repeat("x", 400)))
	if big.GetResponseType() != pb.CommitResponse_SUCCESS {
		t.Fatalf("first: %v", big.GetResponseType())
	}
	if got := a.commitOne(bookmark("c2", "bookmark_bar", strings.Repeat("y", 400))).GetResponseType(); got != pb.CommitResponse_OVER_QUOTA {
		t.Fatalf("over the byte quota: %v", got)
	}
	// Deleting always works and frees room.
	if got := a.commitOne(deleted(bookmark(big.GetIdString(), "bookmark_bar", ""), big.GetVersion())).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("delete: %v", got)
	}
	if got := a.commitOne(bookmark("c3", "bookmark_bar", strings.Repeat("z", 400))).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("after freeing: %v", got)
	}

	chain := chainState(t, s)
	var sum int64
	for _, state := range chain.Types {
		sum += state.Bytes
	}
	if chain.Bytes <= 0 || chain.Bytes != sum || chain.Bytes > 2000 {
		t.Fatalf("chain bytes %d, types %d", chain.Bytes, sum)
	}
	if state := chain.Types[bookmarkType]; state.Tombstones != 1 || state.Entities != 5 {
		t.Fatalf("bookmarks %+v, want 5 live and a tombstone", state)
	}
}

func TestHistoryMakesRoomInsteadOfFailing(t *testing.T) {
	s, _ := newService(t, func(l *Limits) {
		l.MaxEntities = 6 // the permanent entities and one more
		l.MaxHistoryEntities = 10
	})
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(historyType)
	b.updates(historyType)

	for i := range 25 {
		if got := a.commitOne(visit(fmt.Sprint(i), fmt.Sprint(i))).GetResponseType(); got != pb.CommitResponse_SUCCESS {
			t.Fatalf("visit %d: %v", i, got)
		}
	}
	got := b.updates(historyType)
	if len(got) > 10 || len(got) < 5 {
		t.Fatalf("kept %d visits, want at most 10", len(got))
	}
	// The newest visits stay.
	if last := got[len(got)-1].GetSpecifics().GetHistory().GetOriginatorCacheGuid(); last != "24" {
		t.Fatalf("newest visit kept is %q", last)
	}
	if state := chainState(t, s).Types[historyType]; state.Entities != int64(len(got)) || state.Tombstones != 0 {
		t.Fatalf("history state %+v, %d visits", state, len(got))
	}

	// History does not count against the other data types.
	if got := a.commitOne(preference("p", "v")).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("preference: %v", got)
	}
}

func TestHistoryGivesWayToOtherTypes(t *testing.T) {
	s, _ := newService(t, func(l *Limits) {
		l.MaxBytes = 6000
		l.MaxHistoryBytes = 0
		l.MaxHistoryEntities = 0
	})
	a := newDevice(t, s, "a")
	a.updates(historyType)
	for i := range 30 {
		if got := a.commitOne(visit(fmt.Sprint(i), strings.Repeat("h", 150))).GetResponseType(); got != pb.CommitResponse_SUCCESS {
			t.Fatalf("visit %d: %v", i, got)
		}
	}
	if got := a.commitOne(bookmark("c1", "bookmark_bar", strings.Repeat("b", 1500))).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("bookmark with history taking the room: %v", got)
	}
	if chain := chainState(t, s); chain.Bytes > 6000 {
		t.Fatalf("chain holds %d bytes", chain.Bytes)
	}
}

func TestSessionQuota(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxSessionEntities = 2 })
	a := newDevice(t, s, "a")
	a.updates(sessionType)
	a.commitOne(tab("1"))
	a.commitOne(tab("2"))
	if got := a.commitOne(tab("3")).GetResponseType(); got != pb.CommitResponse_OVER_QUOTA {
		t.Fatalf("third tab: %v", got)
	}
	if got := a.commitOne(preference("p", "v")).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("preference: %v", got)
	}
}

func TestHistoryExpires(t *testing.T) {
	s, _ := newService(t, nil)
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(historyType, bookmarkType)
	a.commitOne(visit("v", "visit"))
	a.commitOne(bookmark("c1", "bookmark_bar", "kept"))

	r := store.Retention{ExpiringTypes: ExpiringTypes, ExpiringTypesTTL: 90 * 24 * time.Hour}
	if done := collect(t, s, 30, r); done.Expired != 0 {
		t.Fatalf("collected %+v after 30 days", done)
	}
	if done := collect(t, s, 91, r); done.Expired != 1 {
		t.Fatalf("collected %+v after 91 days, want the visit", done)
	}
	chain := chainState(t, s)
	if state := chain.Types[historyType]; state.Entities != 0 || state.Bytes != 0 || state.PurgedVersion != 0 {
		t.Fatalf("history after expiry %+v", state)
	}
	// Expired visits need no full update: clients expire them too.
	got := b.updates(historyType, bookmarkType)
	if len(got) != 5 {
		t.Fatalf("new device got %d entities, want the 4 bookmark folders and the bookmark", len(got))
	}
	rsp := a.fetch(historyType)
	if rsp.GetGetUpdates().GetNewProgressMarker()[0].GetGcDirective() != nil {
		t.Fatal("expired history sent a garbage collection directive")
	}
}

func TestUnusedChainsAreRemoved(t *testing.T) {
	s, _ := newService(t, nil)
	newDevice(t, s, "a").updates(bookmarkType)
	r := store.Retention{EmptyChainTTL: 7 * 24 * time.Hour, EmptyVersion: EmptyChainVersion}
	if done := collect(t, s, 1, r); done.EmptyChains != 0 {
		t.Fatalf("collected %+v after a day", done)
	}
	if done := collect(t, s, 8, r); done.EmptyChains != 1 {
		t.Fatalf("collected %+v after 8 days", done)
	}
	if n, _ := s.store.CountChains(context.Background()); n != 0 {
		t.Fatalf("%d chains left", n)
	}

	// A chain with data stays, until nobody uses it.
	a := newDevice(t, s, "a")
	a.updates(bookmarkType)
	a.commitOne(bookmark("c1", "bookmark_bar", "x"))
	if done := collect(t, s, 8, r); done.EmptyChains != 0 {
		t.Fatalf("collected %+v of a chain in use", done)
	}
	r = store.Retention{InactiveChainTTL: 365 * 24 * time.Hour}
	if done := collect(t, s, 300, r); done.InactiveChains != 0 {
		t.Fatalf("collected %+v after 300 days", done)
	}
	if done := collect(t, s, 366, r); done.InactiveChains != 1 {
		t.Fatalf("collected %+v after 366 days", done)
	}
}

func TestCreateGate(t *testing.T) {
	allowed := false
	s, _ := newService(t, nil)
	s.allowCreate = func(context.Context) (bool, time.Duration) { return allowed, time.Hour }
	a := newDevice(t, s, "a")
	rsp := a.fetch(bookmarkType)
	if rsp.GetErrorCode() != pb.SyncEnums_THROTTLED || rsp.GetClientCommand().GetThrottleDelaySeconds() != 3600 {
		t.Fatalf("refused creation: %v, delay %d", rsp.GetErrorCode(), rsp.GetClientCommand().GetThrottleDelaySeconds())
	}
	allowed = true
	a.updates(bookmarkType)
	// An existing chain does not ask.
	allowed = false
	if got := a.fetch(bookmarkType).GetErrorCode(); got != pb.SyncEnums_SUCCESS {
		t.Fatalf("existing chain: %v", got)
	}
}
