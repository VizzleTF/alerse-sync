package protocol

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// countingStore counts the reads of changes.
type countingStore struct {
	store.Store
	reads atomic.Int64
}

func (s *countingStore) ChangesSince(ctx context.Context, chainID string, change store.Change, limit int) ([]store.Entity, error) {
	s.reads.Add(1)
	return s.Store.ChangesSince(ctx, chainID, change, limit)
}

func deleted(entity *pb.SyncEntity, version int64) *pb.SyncEntity {
	deletion := proto.CloneOf(entity)
	deletion.Version = proto.Int64(version)
	deletion.Deleted = proto.Bool(true)
	return deletion
}

// collect removes tombstones and expired history as if days had passed.
func collect(t *testing.T, s *Service, days int, r store.Retention) store.Collected {
	t.Helper()
	r.Now = time.Now().Add(time.Duration(days) * 24 * time.Hour)
	r.Batch = 2 // several rounds
	done, err := s.store.Collect(context.Background(), r)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return done
}

func TestFirstSyncLeavesOutTombstones(t *testing.T) {
	s, _ := newService(t, nil)
	a := newDevice(t, s, "a")
	a.updates(bookmarkType)
	kept := a.commitOne(bookmark("c1", "bookmark_bar", "kept"))
	gone := a.commitOne(bookmark("c2", "bookmark_bar", "gone"))
	a.commitOne(deleted(bookmark(gone.GetIdString(), "bookmark_bar", "gone"), gone.GetVersion()))

	b := newDevice(t, s, "b")
	got := b.updates(bookmarkType)
	for _, e := range got {
		if e.GetDeleted() {
			t.Fatalf("first sync got tombstone %v", e)
		}
	}
	if len(got) != 5 || titles(got)[len(titles(got))-4:] != "kept" {
		t.Fatalf("first sync got %q", titles(got))
	}

	// Later changes, deletions included, come as usual.
	b.updates(bookmarkType)
	a.commitOne(deleted(bookmark(kept.GetIdString(), "bookmark_bar", "kept"), kept.GetVersion()))
	if got := b.updates(bookmarkType); len(got) != 1 || !got[0].GetDeleted() {
		t.Fatalf("after deletion got %v", got)
	}
}

func TestFirstSyncInPagesSeesChangesDuringIt(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxUpdates = 3 })
	a := newDevice(t, s, "a")
	a.updates(bookmarkType)
	var entries []*pb.SyncEntity
	for _, name := range []string{"1", "2", "3", "4", "5"} {
		entries = append(entries, bookmark("c"+name, "bookmark_bar", name))
	}
	answers := a.commit(entries...).GetCommit().GetEntryresponse()

	b := newDevice(t, s, "b")
	first := b.fetch(bookmarkType)
	if n := len(first.GetGetUpdates().GetEntries()); n != 3 || first.GetGetUpdates().GetChangesRemaining() == 0 {
		t.Fatalf("first page: %d entities, %d remaining", n, first.GetGetUpdates().GetChangesRemaining())
	}
	// While b downloads, a deletes an entity b already has and one it has
	// not seen yet.
	for _, i := range []int{0, 4} {
		a.commitOne(deleted(bookmark(answers[i].GetIdString(), "bookmark_bar", ""), answers[i].GetVersion()))
	}
	rest := b.updates(bookmarkType)
	live := map[string]bool{}
	for _, e := range append(first.GetGetUpdates().GetEntries(), rest...) {
		live[e.GetIdString()] = !e.GetDeleted()
	}
	for i, answer := range answers {
		if want := i != 0 && i != 4; live[answer.GetIdString()] != want {
			t.Errorf("bookmark %d live %v, want %v", i+1, live[answer.GetIdString()], want)
		}
	}
}

func TestGetUpdatesSkipsUnchangedTypes(t *testing.T) {
	counting := &countingStore{}
	s, _ := newService(t, nil)
	counting.Store = s.store
	s.store = counting
	a := newDevice(t, s, "a")
	all := []int32{bookmarkType, nigoriType, preferenceType, historyType, sessionType}
	a.updates(all...)

	counting.reads.Store(0)
	a.updates(all...)
	if n := counting.reads.Load(); n != 0 {
		t.Fatalf("a request without changes read changes %d times", n)
	}
	a.commitOne(preference("p", "v"))
	if got := a.updates(all...); len(got) != 1 {
		t.Fatalf("got %d entities, want the preference", len(got))
	}
	if n := counting.reads.Load(); n != 1 {
		t.Fatalf("one changed type read changes %d times", n)
	}
	// Every token now stands at the chain's version.
	counting.reads.Store(0)
	a.updates(all...)
	if n := counting.reads.Load(); n != 0 {
		t.Fatalf("read changes %d times after catching up", n)
	}
}

func TestRemovedTombstonesMakeStaleClientsStartOver(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxUpdates = 2 })
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(bookmarkType, preferenceType)
	created := a.commit(bookmark("c1", "bookmark_bar", "one"), bookmark("c2", "bookmark_bar", "two"),
		bookmark("c3", "bookmark_bar", "three")).GetCommit().GetEntryresponse()
	b.updates(bookmarkType, preferenceType)

	// b is away while a deletes a bookmark and the tombstone ages out.
	a.commitOne(deleted(bookmark(created[1].GetIdString(), "bookmark_bar", ""), created[1].GetVersion()))
	a.commitOne(preference("p", "v"))
	a.updates(bookmarkType, preferenceType)
	if done := collect(t, s, 31, store.Retention{TombstoneTTL: 30 * 24 * time.Hour}); done.Tombstones != 1 {
		t.Fatalf("collected %+v, want one tombstone", done)
	}

	// a has seen the deletion and goes on as usual.
	if rsp := a.fetch(bookmarkType); rsp.GetGetUpdates().GetNewProgressMarker()[0].GetGcDirective() != nil {
		t.Fatal("an up-to-date client got a garbage collection directive")
	}

	// b gets every live bookmark again, and a directive to drop the rest,
	// in the first response only.
	first := b.fetch(bookmarkType, preferenceType).GetGetUpdates()
	markers := map[int32]*pb.DataTypeProgressMarker{}
	for _, m := range first.GetNewProgressMarker() {
		markers[m.GetDataTypeId()] = m
	}
	if markers[bookmarkType].GetGcDirective() == nil || markers[preferenceType].GetGcDirective() != nil {
		t.Fatalf("markers %v, want a directive for bookmarks only", first.GetNewProgressMarker())
	}
	bookmarks := first.GetEntries()
	for first.GetChangesRemaining() > 0 {
		next := b.fetch(bookmarkType, preferenceType).GetGetUpdates()
		for _, m := range next.GetNewProgressMarker() {
			if m.GetGcDirective() != nil {
				t.Fatal("a later page repeated the directive")
			}
		}
		bookmarks = append(bookmarks, next.GetEntries()...)
		first = next
	}
	var names []string
	for _, e := range bookmarks {
		if e.GetDeleted() {
			t.Fatalf("full update has a tombstone: %v", e)
		}
		if dataTypeOf(e.GetSpecifics()) == bookmarkType && e.GetServerDefinedUniqueTag() == "" {
			names = append(names, e.GetNonUniqueName())
		}
	}
	if len(names) != 2 || names[0] != "one" || names[1] != "three" {
		t.Fatalf("full update has bookmarks %v, want one, three", names)
	}
	if got := b.updates(bookmarkType); len(got) != 0 {
		t.Fatalf("after the full update got %d entities", len(got))
	}
}
