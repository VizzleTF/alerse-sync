package protocol

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

const chainID = "chain"

type notification struct {
	chain, origin string
	dataTypes     []int32
}

type recorder struct{ got []notification }

func (r *recorder) Notify(_ context.Context, chainID, origin string, dataTypes []int32) {
	r.got = append(r.got, notification{chainID, origin, dataTypes})
}

func newService(t *testing.T, change func(*Limits)) (*Service, *recorder) {
	t.Helper()
	limits := DefaultLimits
	if change != nil {
		change(&limits)
	}
	rec := &recorder{}
	return New(memory.New(), rec, limits), rec
}

// device is a client of the test chain that keeps its birthday and progress
// tokens like Chromium does.
type device struct {
	t        *testing.T
	service  *Service
	guid     string
	birthday string
	tokens   map[int32][]byte
}

func newDevice(t *testing.T, s *Service, guid string) *device {
	return &device{t: t, service: s, guid: guid, tokens: map[int32][]byte{}}
}

func (d *device) send(msg *pb.ClientToServerMessage) *pb.ClientToServerResponse {
	d.t.Helper()
	msg.Share = proto.String("")
	if d.birthday != "" {
		msg.StoreBirthday = proto.String(d.birthday)
	}
	rsp, err := d.service.Handle(context.Background(), chainID, msg)
	if err != nil {
		d.t.Fatalf("Handle: %v", err)
	}
	if rsp.GetStoreBirthday() != "" {
		d.birthday = rsp.GetStoreBirthday()
	}
	return rsp
}

// fetch asks for updates of dataTypes once and records the new tokens.
func (d *device) fetch(dataTypes ...int32) *pb.ClientToServerResponse {
	d.t.Helper()
	request := &pb.GetUpdatesMessage{
		GetUpdatesOrigin:  pb.SyncEnums_PERIODIC.Enum(),
		NeedEncryptionKey: proto.Bool(true),
	}
	for _, dataType := range dataTypes {
		request.FromProgressMarker = append(request.FromProgressMarker,
			&pb.DataTypeProgressMarker{DataTypeId: proto.Int32(dataType), Token: d.tokens[dataType]})
	}
	rsp := d.send(&pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_GET_UPDATES.Enum(),
		GetUpdates:      request,
	})
	for _, marker := range rsp.GetGetUpdates().GetNewProgressMarker() {
		d.tokens[marker.GetDataTypeId()] = marker.GetToken()
	}
	return rsp
}

// updates fetches until nothing remains and returns the entities.
func (d *device) updates(dataTypes ...int32) []*pb.SyncEntity {
	d.t.Helper()
	var entities []*pb.SyncEntity
	for {
		rsp := d.fetch(dataTypes...)
		if rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
			d.t.Fatalf("GetUpdates: %v", rsp.GetErrorCode())
		}
		entities = append(entities, rsp.GetGetUpdates().GetEntries()...)
		if rsp.GetGetUpdates().GetChangesRemaining() == 0 {
			return entities
		}
	}
}

func (d *device) commit(entries ...*pb.SyncEntity) *pb.ClientToServerResponse {
	d.t.Helper()
	return d.send(&pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_COMMIT.Enum(),
		Commit:          &pb.CommitMessage{Entries: entries, CacheGuid: proto.String(d.guid)},
	})
}

// commitOne commits entry and returns its answer.
func (d *device) commitOne(entry *pb.SyncEntity) *pb.CommitResponse_EntryResponse {
	d.t.Helper()
	rsp := d.commit(entry)
	if rsp.GetErrorCode() != pb.SyncEnums_SUCCESS {
		d.t.Fatalf("Commit: %v", rsp.GetErrorCode())
	}
	return rsp.GetCommit().GetEntryresponse()[0]
}

func bookmark(id, parent, title string) *pb.SyncEntity {
	return &pb.SyncEntity{
		IdString:       proto.String(id),
		ParentIdString: proto.String(parent),
		NonUniqueName:  proto.String(title),
		Specifics: &pb.EntitySpecifics{SpecificsVariant: &pb.EntitySpecifics_Bookmark{
			Bookmark: &pb.BookmarkSpecifics{LegacyCanonicalizedTitle: proto.String(title)},
		}},
	}
}

func preference(tag, value string) *pb.SyncEntity {
	return &pb.SyncEntity{
		IdString:      proto.String("c-" + tag),
		ClientTagHash: proto.String(tag),
		Specifics: &pb.EntitySpecifics{SpecificsVariant: &pb.EntitySpecifics_Preference{
			Preference: &pb.PreferenceSpecifics{Name: proto.String(tag), Value: proto.String(value)},
		}},
	}
}

const preferenceType = 37702

func byTag(entities []*pb.SyncEntity) map[string]*pb.SyncEntity {
	tags := map[string]*pb.SyncEntity{}
	for _, e := range entities {
		tags[e.GetServerDefinedUniqueTag()] = e
	}
	return tags
}

func titles(entities []*pb.SyncEntity) string {
	var names []string
	for _, e := range entities {
		names = append(names, e.GetNonUniqueName())
	}
	return strings.Join(names, ",")
}

func TestFirstSyncCreatesPermanentEntities(t *testing.T) {
	s, _ := newService(t, nil)
	d := newDevice(t, s, "a")
	rsp := d.fetch(bookmarkType, nigoriType)

	if d.birthday == "" {
		t.Fatal("no store birthday")
	}
	if keys := rsp.GetGetUpdates().GetEncryptionKeys(); len(keys) != 1 || len(keys[0]) != 16 {
		t.Fatalf("encryption keys %x, want one 16-byte key", keys)
	}
	tags := byTag(rsp.GetGetUpdates().GetEntries())
	for _, p := range permanentEntities {
		e, ok := tags[p.tag]
		if !ok {
			t.Fatalf("permanent entity %s missing", p.tag)
		}
		if e.GetParentIdString() != p.parent || e.GetIdString() != p.tag || dataTypeOf(e.GetSpecifics()) != p.dataType {
			t.Fatalf("permanent entity %s: %v", p.tag, e)
		}
	}

	// A second device of the chain gets the same chain.
	other := newDevice(t, s, "b")
	other.fetch(bookmarkType)
	if other.birthday != d.birthday {
		t.Fatal("second device got another birthday")
	}
	if got := d.updates(bookmarkType, nigoriType); len(got) != 0 {
		t.Fatalf("got %d entities again", len(got))
	}
}

func TestCommitAndFetch(t *testing.T) {
	s, rec := newService(t, nil)
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(bookmarkType)
	b.updates(bookmarkType)

	// A folder and its child in one commit: the child names the folder by
	// the client's ID.
	rsp := a.commit(bookmark("c1", "bookmark_bar", "Folder"), bookmark("c2", "c1", "Child"))
	answers := rsp.GetCommit().GetEntryresponse()
	if len(answers) != 2 {
		t.Fatalf("%d answers, want 2", len(answers))
	}
	for _, answer := range answers {
		if answer.GetResponseType() != pb.CommitResponse_SUCCESS {
			t.Fatalf("commit: %v %s", answer.GetResponseType(), answer.GetErrorMessage())
		}
	}
	folderID := answers[0].GetIdString()

	got := b.updates(bookmarkType)
	if titles(got) != "Folder,Child" {
		t.Fatalf("updates %q, want Folder,Child", titles(got))
	}
	child := got[1]
	if child.GetParentIdString() != folderID {
		t.Fatalf("child's parent %q, want %q", child.GetParentIdString(), folderID)
	}
	if child.GetOriginatorCacheGuid() != "a" || child.GetOriginatorClientItemId() != "c2" {
		t.Fatalf("originator %q/%q, want a/c2", child.GetOriginatorCacheGuid(), child.GetOriginatorClientItemId())
	}
	if child.GetVersion() != answers[1].GetVersion() || child.GetCtime() == 0 {
		t.Fatalf("child version %d ctime %d", child.GetVersion(), child.GetCtime())
	}

	want := notification{chainID, "a", []int32{bookmarkType}}
	if len(rec.got) != 1 || rec.got[0].origin != want.origin || rec.got[0].dataTypes[0] != bookmarkType {
		t.Fatalf("notifications %v, want %v", rec.got, want)
	}
}

func TestUpdateConflict(t *testing.T) {
	s, _ := newService(t, nil)
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(bookmarkType)
	created := a.commitOne(bookmark("c1", "bookmark_bar", "One"))
	b.updates(bookmarkType)

	edit := bookmark(created.GetIdString(), "bookmark_bar", "Two")
	edit.Version = proto.Int64(created.GetVersion())
	if got := a.commitOne(edit).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("first edit: %v", got)
	}
	// b edits the version a already replaced.
	if got := b.commitOne(edit).GetResponseType(); got != pb.CommitResponse_CONFLICT {
		t.Fatalf("stale edit: %v, want CONFLICT", got)
	}
	if got := titles(b.updates(bookmarkType)); got != "Two" {
		t.Fatalf("b sees %q, want Two", got)
	}
}

func TestClientTags(t *testing.T) {
	s, _ := newService(t, nil)
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(preferenceType)
	b.updates(preferenceType)

	first := a.commitOne(preference("home", "1"))
	if first.GetResponseType() != pb.CommitResponse_SUCCESS {
		t.Fatalf("create: %v", first.GetResponseType())
	}
	// Both devices created the same preference: one entity, one conflict.
	if got := b.commitOne(preference("home", "2")).GetResponseType(); got != pb.CommitResponse_CONFLICT {
		t.Fatalf("second create: %v, want CONFLICT", got)
	}

	deletion := preference("home", "")
	deletion.Version = proto.Int64(first.GetVersion())
	deletion.Deleted = proto.Bool(true)
	if got := a.commitOne(deletion).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("delete: %v", got)
	}
	tombstone := b.updates(preferenceType)
	if len(tombstone) != 1 || !tombstone[0].GetDeleted() ||
		tombstone[0].GetSpecifics().GetPreference().GetValue() != "" {
		t.Fatalf("tombstone %v", tombstone)
	}

	// A deleted entity is created again from version 0, under the same ID.
	again := b.commitOne(preference("home", "3"))
	if again.GetResponseType() != pb.CommitResponse_SUCCESS || again.GetIdString() != first.GetIdString() {
		t.Fatalf("create again: %v %s", again.GetResponseType(), again.GetIdString())
	}
}

func TestBirthday(t *testing.T) {
	s, _ := newService(t, nil)
	a := newDevice(t, s, "a")

	// A commit before the first GetUpdates has no birthday.
	if got := a.commit(bookmark("c1", "bookmark_bar", "x")).GetErrorCode(); got != pb.SyncEnums_NOT_MY_BIRTHDAY {
		t.Fatalf("commit without birthday: %v", got)
	}
	a.updates(bookmarkType)
	a.birthday = "other"
	if got := a.fetch(bookmarkType).GetErrorCode(); got != pb.SyncEnums_NOT_MY_BIRTHDAY {
		t.Fatalf("wrong birthday: %v", got)
	}
}

func TestClearServerData(t *testing.T) {
	s, rec := newService(t, nil)
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(bookmarkType)
	b.updates(bookmarkType)
	a.commitOne(bookmark("c1", "bookmark_bar", "x"))
	old := a.birthday

	rsp := a.send(&pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_CLEAR_SERVER_DATA.Enum(),
		ClearServerData: &pb.ClearServerDataMessage{},
	})
	if rsp.GetErrorCode() != pb.SyncEnums_SUCCESS || rsp.GetClearServerData() == nil {
		t.Fatalf("clear: %v", rsp.GetErrorCode())
	}
	if last := rec.got[len(rec.got)-1]; last.dataTypes[0] != nigoriType {
		t.Fatalf("clear notified %v", last)
	}
	if got := b.fetch(bookmarkType).GetErrorCode(); got != pb.SyncEnums_NOT_MY_BIRTHDAY {
		t.Fatalf("old device after clear: %v", got)
	}

	// Starting over gives a new chain.
	fresh := newDevice(t, s, "c")
	got := fresh.updates(bookmarkType)
	if fresh.birthday == old || len(got) != 4 {
		t.Fatalf("after clear: birthday %q (old %q), %d entities", fresh.birthday, old, len(got))
	}
}

func TestPaging(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxUpdates = 3 })
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(bookmarkType, preferenceType)
	b.updates(bookmarkType, preferenceType)

	var entries []*pb.SyncEntity
	for _, name := range []string{"1", "2", "3", "4", "5"} {
		entries = append(entries, bookmark("c"+name, "bookmark_bar", name))
	}
	a.commit(entries...)
	a.commit(preference("p", "v"))

	first := b.fetch(bookmarkType, preferenceType).GetGetUpdates()
	if len(first.GetEntries()) != 3 || first.GetChangesRemaining() != 3 {
		t.Fatalf("first page: %d entities, %d remaining; want 3, 3",
			len(first.GetEntries()), first.GetChangesRemaining())
	}
	rest := b.updates(bookmarkType, preferenceType)
	if titles(rest) != "4,5," || rest[2].GetSpecifics().GetPreference().GetValue() != "v" {
		t.Fatalf("rest %q", titles(rest))
	}
}

func TestQuota(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxEntities = 6 }) // 5 permanent
	a := newDevice(t, s, "a")
	a.updates(bookmarkType)

	kept := a.commitOne(preference("one", "1"))
	if got := a.commitOne(preference("two", "2")).GetResponseType(); got != pb.CommitResponse_OVER_QUOTA {
		t.Fatalf("over quota: %v", got)
	}
	deletion := preference("one", "")
	deletion.Version = proto.Int64(kept.GetVersion())
	deletion.Deleted = proto.Bool(true)
	a.commitOne(deletion)
	if got := a.commitOne(preference("two", "2")).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("after freeing: %v", got)
	}
}

func TestPermanentEntities(t *testing.T) {
	s, _ := newService(t, nil)
	a := newDevice(t, s, "a")
	tags := byTag(a.updates(bookmarkType, nigoriType))

	bar := proto.CloneOf(tags["bookmark_bar"])
	bar.Deleted = proto.Bool(true)
	if got := a.commitOne(bar).GetResponseType(); got != pb.CommitResponse_INVALID_MESSAGE {
		t.Fatalf("delete bookmark bar: %v", got)
	}
	forged := bookmark("c1", "bookmark_bar", "x")
	forged.ServerDefinedUniqueTag = proto.String("mine")
	if got := a.commitOne(forged).GetResponseType(); got != pb.CommitResponse_INVALID_MESSAGE {
		t.Fatalf("create permanent: %v", got)
	}

	// Chromium commits its encryption state to the Nigori node.
	nigori := proto.CloneOf(tags["google_chrome_nigori"])
	nigori.Name = proto.String("renamed")
	nigori.Specifics = &pb.EntitySpecifics{SpecificsVariant: &pb.EntitySpecifics_Nigori{
		Nigori: &pb.NigoriSpecifics{KeystoreMigrationTime: proto.Int64(1)},
	}}
	if got := a.commitOne(nigori).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("update nigori: %v", got)
	}
	updated := a.updates(nigoriType)
	if len(updated) != 1 || updated[0].GetServerDefinedUniqueTag() != "google_chrome_nigori" ||
		updated[0].GetName() != "Nigori" || updated[0].GetSpecifics().GetNigori().GetKeystoreMigrationTime() != 1 {
		t.Fatalf("nigori after update: %v", updated)
	}
}

func TestInvalidEntities(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxEntityBytes = 100 })
	a := newDevice(t, s, "a")
	a.updates(bookmarkType)

	untyped := &pb.SyncEntity{IdString: proto.String("c1")}
	large := bookmark("c2", "bookmark_bar", strings.Repeat("x", 200))
	retyped := preference("p", "v")
	created := a.commitOne(retyped)
	retyped = bookmark(created.GetIdString(), "bookmark_bar", "x")
	retyped.Version = proto.Int64(created.GetVersion())

	for name, entry := range map[string]*pb.SyncEntity{"untyped": untyped, "large": large, "retyped": retyped} {
		if got := a.commitOne(entry).GetResponseType(); got != pb.CommitResponse_INVALID_MESSAGE {
			t.Errorf("%s: %v, want INVALID_MESSAGE", name, got)
		}
	}
}

func TestTokens(t *testing.T) {
	for _, v := range []int64{0, 1, 1 << 40} {
		if got := decodeToken(encodeToken(v)); got != v {
			t.Errorf("token of %d decodes to %d", v, got)
		}
	}
	for _, token := range [][]byte{nil, {}, []byte("garbage"), {2, 0, 0, 0, 0, 0, 0, 0, 1}} {
		if got := decodeToken(token); got != 0 {
			t.Errorf("token %x decodes to %d, want 0", token, got)
		}
	}
}

func TestNotMyBirthdayLetsChromiumStartOver(t *testing.T) {
	s, _ := newService(t, nil)
	a := newDevice(t, s, "a")
	a.updates(bookmarkType)
	current := a.birthday
	a.birthday = "old"

	rsp := a.fetch(bookmarkType)
	// Chromium derives "turn sync off and clear data" from the error code and
	// a differing birthday; an explicit error field would override that.
	if rsp.GetErrorCode() != pb.SyncEnums_NOT_MY_BIRTHDAY || rsp.Error != nil {
		t.Fatalf("error code %v, error field %v", rsp.GetErrorCode(), rsp.Error)
	}
	if rsp.GetStoreBirthday() != current {
		t.Fatalf("birthday %q, want the chain's %q", rsp.GetStoreBirthday(), current)
	}
}

func TestUnknownDataType(t *testing.T) {
	s, _ := newService(t, nil)
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	const futureType = 9_999_999
	a.updates(futureType)
	b.updates(futureType)

	// A data type newer than the server's protocol definitions.
	specifics := &pb.EntitySpecifics{}
	specifics.ProtoReflect().SetUnknown(protowire.AppendBytes(
		protowire.AppendTag(nil, futureType, protowire.BytesType), []byte("payload")))
	entry := &pb.SyncEntity{IdString: proto.String("c1"), ClientTagHash: proto.String("t"), Specifics: specifics}
	if got := a.commitOne(entry).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("commit: %v", got)
	}
	got := b.updates(futureType)
	if len(got) != 1 || !bytes.Contains(got[0].GetSpecifics().ProtoReflect().GetUnknown(), []byte("payload")) {
		t.Fatalf("fetched %v", got)
	}
	if dataTypeOf(emptySpecifics(futureType)) != futureType {
		t.Fatal("tombstone of an unknown type lost its type")
	}
}

func TestCommitOnlyTypesAreNotStored(t *testing.T) {
	s, rec := newService(t, nil)
	a := newDevice(t, s, "a")
	const userEventType = 455206
	a.updates(userEventType)
	before, _, _ := s.store.Chain(context.Background(), chainID)

	event := &pb.SyncEntity{
		IdString: proto.String("e1"),
		Specifics: &pb.EntitySpecifics{SpecificsVariant: &pb.EntitySpecifics_UserEvent{
			UserEvent: &pb.UserEventSpecifics{EventTimeUsec: proto.Int64(1)},
		}},
	}
	if got := a.commitOne(event).GetResponseType(); got != pb.CommitResponse_SUCCESS {
		t.Fatalf("commit: %v", got)
	}
	after, _, _ := s.store.Chain(context.Background(), chainID)
	if after.LiveEntities != before.LiveEntities || len(rec.got) != 0 {
		t.Fatalf("user event stored: %d → %d entities, %d notifications",
			before.LiveEntities, after.LiveEntities, len(rec.got))
	}
}

func TestResponseByteBudget(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxUpdateBytes = 300 })
	a, b := newDevice(t, s, "a"), newDevice(t, s, "b")
	a.updates(bookmarkType)
	b.updates(bookmarkType)
	for _, name := range []string{"1", "2", "3", "4"} {
		a.commitOne(bookmark("c"+name, "bookmark_bar", strings.Repeat(name, 100)))
	}

	first := b.fetch(bookmarkType).GetGetUpdates()
	if n := len(first.GetEntries()); n == 0 || n == 4 || first.GetChangesRemaining() != int64(4-n) {
		t.Fatalf("first page: %d entities, %d remaining", n, first.GetChangesRemaining())
	}
	if got := len(first.GetEntries()) + len(b.updates(bookmarkType)); got != 4 {
		t.Fatalf("fetched %d bookmarks in all, want 4", got)
	}
}

func TestMaxChains(t *testing.T) {
	s, _ := newService(t, func(l *Limits) { l.MaxChains = 1 })
	newDevice(t, s, "a").updates(bookmarkType)

	rsp, err := s.Handle(context.Background(), "another chain", &pb.ClientToServerMessage{
		MessageContents: pb.ClientToServerMessage_GET_UPDATES.Enum(),
		GetUpdates:      &pb.GetUpdatesMessage{},
	})
	if err != nil || rsp.GetErrorCode() != pb.SyncEnums_TRANSIENT_ERROR {
		t.Fatalf("second chain: %v, %v", rsp.GetErrorCode(), err)
	}
}
