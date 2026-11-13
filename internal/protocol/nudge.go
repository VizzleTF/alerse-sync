package protocol

import (
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

// keepNudgeDelay names the data types whose own delay Chromium keeps: those
// waiting longer already (autofill, user events), and those a person waits
// for (encryption keys at setup, device info, messages to other devices).
var keepNudgeDelay = []string{
	"autofill", "user_event", "nigori", "device_info", "send_tab_to_self",
	"sharing_message", "outgoing_password_sharing_invitation",
	"incoming_password_sharing_invitation",
}

// nudgeDelays tells clients to wait delay before they commit a local change
// of any data type but those above, and sessions for open tabs. Chromium
// starts the wait at the first change and sends all that came meanwhile in
// one commit, and applies the server's delays as they are.
func nudgeDelays(delay, sessions time.Duration) []*pb.CustomNudgeDelay {
	var delays []*pb.CustomNudgeDelay
	add := func(dataType int32, d time.Duration) {
		if d > 0 {
			delays = append(delays, &pb.CustomNudgeDelay{
				DatatypeId: proto.Int32(dataType), DelayMs: proto.Int32(int32(d / time.Millisecond)),
			})
		}
	}
	fields := (&pb.EntitySpecifics{}).ProtoReflect().Descriptor().Oneofs().ByName("specifics_variant").Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		number := int32(f.Number())
		switch {
		case number == sessionType:
			add(number, sessions)
		case !slices.Contains(keepNudgeDelay, string(f.Name())):
			add(number, delay)
		}
	}
	return delays
}
