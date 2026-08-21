package protocol

import (
	"errors"

	"google.golang.org/protobuf/proto"
)

var (
	// ErrCorruptStreamHistory is returned when a stored history window cannot
	// be decoded, or decodes into something that violates the type's
	// invariants (bad header, non-monotonic timestamps, over-capacity).
	//
	// Stored state is untrusted input: callers must handle this by discarding
	// the window and re-warming, never by panicking.
	ErrCorruptStreamHistory = errors.New("corrupt stream history")

	// ErrInsufficientStreamHistory is returned when fewer records are stored
	// than were asked for. It means "not yet evaluable" and must never be
	// treated as zero or as a shorter window.
	ErrInsufficientStreamHistory = errors.New("insufficient stream history")

	// ErrHistoryRecordTooLarge is returned when a value serializes to more than
	// MaxHistoryRecordBytes. Rejecting the record leaves an honest gap in the
	// series; accepting it would let one pair's window grow past what the
	// per-round byte budget was sized for.
	ErrHistoryRecordTooLarge = errors.New("history record too large")
)

// deterministicMarshal is the marshaller for anything whose bytes are compared
// across oracles: replicated KeyValueState (history headers and chunks), and the
// serialized stream values ModeAggregator counts to pick a mode. A
// non-deterministic encoding there would halt the DON.
//
// None of the messages marshalled through it has a map field today, and
// Deterministic only orders map entries, so it is currently a no-op. It is
// applied anyway so that adding a map field to one of them cannot silently
// introduce a divergence.
var deterministicMarshal = proto.MarshalOptions{Deterministic: true}

// StreamHistoryRecord is one agreed aggregate value of a stream together with
// the timestamp it was observed at.
type StreamHistoryRecord struct {
	ObservedAtNanoseconds uint64
	Value                 StreamValue
}

// historyRecordSize returns the serialized size of one history record, so the
// per-record cap is enforced against what is actually stored rather than an
// assumed size.
func historyRecordSize(observedAtNanoseconds uint64, value StreamValue) (int, error) {
	pb, err := marshalProtoStreamValue(value)
	if err != nil {
		return 0, err
	}
	return proto.Size(&LLOStreamHistoryRecord{
		ObservedAtNanoseconds: observedAtNanoseconds,
		Value:                 pb,
	}), nil
}

// marshalProtoStreamValue converts a StreamValue to its tagged wire form.
func marshalProtoStreamValue(sv StreamValue) (*LLOStreamValue, error) {
	if sv == nil {
		return nil, ErrNilStreamValue
	}
	value, err := sv.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return &LLOStreamValue{Type: sv.Type(), Value: value}, nil
}
