package protocol

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// FuzzUnmarshalObservedProtoStreamValue feeds arbitrary bytes through the
// observation decode path. Observations are attacker-controlled, so the contract
// is that any input either decodes into a value within every bound or returns an
// error -- never a panic, and never an accepted value over a bound.
func FuzzUnmarshalObservedProtoStreamValue(f *testing.F) {
	for _, typ := range []LLOStreamValue_Type{
		LLOStreamValue_Decimal,
		LLOStreamValue_Quote,
		LLOStreamValue_TimestampedStreamValue,
	} {
		for _, body := range [][]byte{nil, {}, {0x01}, {0xff, 0xff, 0xff, 0xff}} {
			b, err := proto.Marshal(&LLOStreamValue{Type: typ, Value: body})
			if err != nil {
				f.Fatal(err)
			}
			f.Add(b)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		enc := &LLOStreamValue{}
		if err := proto.Unmarshal(data, enc); err != nil {
			return // not a stream value proto; nothing to check
		}
		sv, err := UnmarshalObservedProtoStreamValue(enc)
		if err != nil {
			return
		}
		if sv == nil {
			t.Fatal("decoded a nil stream value without an error")
		}
		// An accepted value must satisfy the bounds the decoder claims to
		// enforce, at every nesting level.
		if err := checkObservedStreamValue(sv); err != nil {
			t.Fatalf("accepted a value that violates its own bounds: %v", err)
		}
	})
}
