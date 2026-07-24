package llo

import (
	"errors"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
)

// Conversion glue between LLO domain types and the shared generated protobuf
// types. These mirror the equivalent (unexported) helpers in v30; v31 keeps its
// own copies to stay self-contained.

func makeLLOStreamValue(v llocommon.StreamValue) (*llocommon.LLOStreamValue, error) {
	if v == nil {
		return nil, errors.New("nil value for stream")
	}
	b, err := v.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return &llocommon.LLOStreamValue{Type: v.Type(), Value: b}, nil
}

func makeChannelDefinitionProto(d llotypes.ChannelDefinition) *llocommon.LLOChannelDefinitionProto {
	streams := make([]*llocommon.LLOStreamDefinition, len(d.Streams))
	for i, strm := range d.Streams {
		streams[i] = &llocommon.LLOStreamDefinition{
			StreamID:   strm.StreamID,
			Aggregator: uint32(strm.Aggregator),
		}
	}
	return &llocommon.LLOChannelDefinitionProto{
		ReportFormat:           uint32(d.ReportFormat),
		Streams:                streams,
		Opts:                   d.Opts,
		Tombstone:              d.Tombstone,
		Source:                 d.Source,
		DisableNilStreamValues: d.DisableNilStreamValues,
	}
}

func channelDefinitionFromProto(pb *llocommon.LLOChannelDefinitionProto) llotypes.ChannelDefinition {
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
