package llo

import (
	"errors"
	"fmt"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// OBSERVATION CODEC
// NOTE: These codecs make a lot of allocations which will be hard on the
// garbage collector, this can probably be made more efficient

var (
	_ ObservationCodec = (*protoObservationCodec)(nil)
)

type protoObservationCodec struct{}

func (c protoObservationCodec) Encode(obs Observation) (types.Observation, error) {
	dfns := channelDefinitionsToProtoObservation(obs.UpdateChannelDefinitions)

	streamValues := make(map[uint32]*LLOStreamValue, len(obs.StreamValues))
	for id, sv := range obs.StreamValues {
		if sv != nil {
			enc, err := sv.MarshalBinary()
			if errors.Is(err, ErrNilStreamValue) {
				// Ignore nil values
				continue
			} else if err != nil {
				return nil, fmt.Errorf("failed to encode observation: %w", err)
			}
			streamValues[id] = &LLOStreamValue{
				Type:  sv.Type(),
				Value: enc,
			}
		}
	}

	pbuf := &LLOObservationProto{
		AttestedPredecessorRetirement:  obs.AttestedPredecessorRetirement,
		ShouldRetire:                   obs.ShouldRetire,
		UnixTimestampNanoseconds:       obs.UnixTimestampNanoseconds,
		UnixTimestampNanosecondsLegacy: int64(obs.UnixTimestampNanoseconds), //nolint:gosec // this won't overflow
		RemoveChannelIDs:               maps.Keys(obs.RemoveChannelIDs),
		UpdateChannelDefinitions:       dfns,
		StreamValues:                   streamValues,
	}

	return proto.Marshal(pbuf)
}

func channelDefinitionsToProtoObservation(in llotypes.ChannelDefinitions) (out map[uint32]*LLOChannelDefinitionProto) {
	if len(in) > 0 {
		out = make(map[uint32]*LLOChannelDefinitionProto, len(in))
		for id, d := range in {
			streams := make([]*LLOStreamDefinition, len(d.Streams))
			for i, strm := range d.Streams {
				streams[i] = &LLOStreamDefinition{
					StreamID:   strm.StreamID,
					Aggregator: uint32(strm.Aggregator),
				}
			}
			out[id] = &LLOChannelDefinitionProto{
				ReportFormat: uint32(d.ReportFormat),
				Streams:      streams,
				Opts:         d.Opts,
			}
		}
	}
	return
}

func (c protoObservationCodec) Decode(b types.Observation) (Observation, error) {
	pbuf := &LLOObservationProto{}
	err := proto.Unmarshal(b, pbuf)
	if err != nil {
		return Observation{}, fmt.Errorf("failed to decode observation: expected protobuf (got: 0x%x); %w", b, err)
	}
	var removeChannelIDs map[llotypes.ChannelID]struct{}
	if len(pbuf.RemoveChannelIDs) > 0 {
		removeChannelIDs = make(map[llotypes.ChannelID]struct{}, len(pbuf.RemoveChannelIDs))
		for _, id := range pbuf.RemoveChannelIDs {
			if _, exists := removeChannelIDs[id]; exists {
				// Byzantine behavior makes this observation invalid; a
				// well-behaved node should never encode duplicates here
				return Observation{}, fmt.Errorf("failed to decode observation; duplicate channel ID in RemoveChannelIDs: %d", id)
			}
			removeChannelIDs[id] = struct{}{}
		}
	}
	dfns := channelDefinitionsFromProtoObservation(pbuf.UpdateChannelDefinitions)
	var streamValues StreamValues
	if len(pbuf.StreamValues) > 0 {
		streamValues = make(StreamValues, len(pbuf.StreamValues))
		for id, enc := range pbuf.StreamValues {
			sv, err := UnmarshalProtoStreamValue(enc)
			if err != nil {
				// Byzantine behavior makes this observation invalid; a
				// well-behaved node should never encode invalid or nil values
				// here
				return Observation{}, fmt.Errorf("failed to decode observation; invalid stream value for stream ID: %d; %w", id, err)
			}
			streamValues[id] = sv
		}
	}
	var unixTSNanoseconds uint64
	if pbuf.UnixTimestampNanoseconds > 0 {
		unixTSNanoseconds = pbuf.UnixTimestampNanoseconds
	} else if pbuf.UnixTimestampNanosecondsLegacy >= 0 {
		unixTSNanoseconds = uint64(pbuf.UnixTimestampNanosecondsLegacy)
	} else {
		// Byzantine behavior makes this observation invalid; a well-behaved
		// node should never encode negative timestamps here
		return Observation{}, fmt.Errorf("failed to decode observation; cannot accept negative unix timestamp: %d", pbuf.UnixTimestampNanosecondsLegacy)
	}
	obs := Observation{
		AttestedPredecessorRetirement: pbuf.AttestedPredecessorRetirement,
		ShouldRetire:                  pbuf.ShouldRetire,
		UnixTimestampNanoseconds:      unixTSNanoseconds,
		RemoveChannelIDs:              removeChannelIDs,
		UpdateChannelDefinitions:      dfns,
		StreamValues:                  streamValues,
	}
	return obs, nil
}

func channelDefinitionsFromProtoObservation(channelDefinitions map[uint32]*LLOChannelDefinitionProto) llotypes.ChannelDefinitions {
	if len(channelDefinitions) == 0 {
		return nil
	}
	dfns := make(map[llotypes.ChannelID]llotypes.ChannelDefinition, len(channelDefinitions))
	for id, d := range channelDefinitions {
		streams := make([]llotypes.Stream, len(d.Streams))
		for i, strm := range d.Streams {
			streams[i] = llotypes.Stream{
				StreamID:   strm.StreamID,
				Aggregator: llotypes.Aggregator(strm.Aggregator),
			}
		}
		dfns[id] = llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormat(d.ReportFormat),
			Streams:      streams,
			Opts:         d.Opts,
		}
	}
	return dfns
}
