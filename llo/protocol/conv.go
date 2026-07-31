package protocol

import (
	"errors"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// Conversion glue between LLO domain types and the shared generated protobuf
// types. Shared across all LLO plugin versions (v30, v31, ...).

// StreamValueToProto encodes a StreamValue into its protobuf wire form.
func StreamValueToProto(v StreamValue) (*LLOStreamValue, error) {
	if v == nil {
		return nil, errors.New("nil value for stream")
	}
	b, err := v.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return &LLOStreamValue{Type: v.Type(), Value: b}, nil
}

// ChannelDefinitionToProto encodes a ChannelDefinition into its protobuf form.
func ChannelDefinitionToProto(d llotypes.ChannelDefinition) *LLOChannelDefinitionProto {
	streams := make([]*LLOStreamDefinition, len(d.Streams))
	for i, strm := range d.Streams {
		streams[i] = &LLOStreamDefinition{
			StreamID:   strm.StreamID,
			Aggregator: uint32(strm.Aggregator),
		}
	}
	return &LLOChannelDefinitionProto{
		ReportFormat:           uint32(d.ReportFormat),
		Streams:                streams,
		Opts:                   d.Opts,
		Tombstone:              d.Tombstone,
		Source:                 d.Source,
		DisableNilStreamValues: d.DisableNilStreamValues,
	}
}

// ChannelDefinitionFromProto decodes a protobuf ChannelDefinition. The caller
// must ensure pb is non-nil.
func ChannelDefinitionFromProto(pb *LLOChannelDefinitionProto) llotypes.ChannelDefinition {
	streams := make([]llotypes.Stream, len(pb.Streams))
	for i, strm := range pb.Streams {
		streams[i] = llotypes.Stream{
			StreamID:   strm.StreamID,
			Aggregator: llotypes.Aggregator(strm.Aggregator),
		}
	}
	return llotypes.ChannelDefinition{
		ReportFormat:           llotypes.ReportFormat(pb.ReportFormat),
		Streams:                streams,
		Opts:                   pb.Opts,
		Tombstone:              pb.Tombstone,
		Source:                 pb.Source,
		DisableNilStreamValues: pb.DisableNilStreamValues,
	}
}
