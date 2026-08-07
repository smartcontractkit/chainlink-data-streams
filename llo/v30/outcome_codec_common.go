package llo

import (
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Common functions shared between outcome codecs

func StreamAggregatesToProtoOutcome(in protocol.StreamAggregates) (out []*protocol.LLOStreamAggregate, err error) {
	if len(in) > 0 {
		out = make([]*protocol.LLOStreamAggregate, 0, len(in))
		for sid, aggregates := range in {
			if aggregates == nil {
				return nil, fmt.Errorf("cannot marshal protobuf; nil aggregates for stream ID: %d", sid)
			}
			for agg, v := range aggregates {
				pbSv, err := protocol.StreamValueToProto(v)
				if err != nil {
					return nil, fmt.Errorf("cannot marshal protobuf; stream ID: %d; aggregator: %v; %w", sid, agg, err)
				}
				out = append(out, &protocol.LLOStreamAggregate{
					StreamID:    sid,
					StreamValue: pbSv,
					Aggregator:  uint32(agg),
				})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].StreamID == out[j].StreamID {
				return out[i].Aggregator < out[j].Aggregator
			}
			return out[i].StreamID < out[j].StreamID
		})
	}
	return
}

func channelDefinitionsToProtoOutcome(in llotypes.ChannelDefinitions) (out []*protocol.LLOChannelIDAndDefinitionProto) {
	if len(in) > 0 {
		out = make([]*protocol.LLOChannelIDAndDefinitionProto, 0, len(in))
		for id, d := range in {
			out = append(out, &protocol.LLOChannelIDAndDefinitionProto{
				ChannelID:         id,
				ChannelDefinition: protocol.ChannelDefinitionToProto(d),
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].ChannelID < out[j].ChannelID
		})
	}
	return
}

func channelDefinitionsFromProtoOutcome(in []*protocol.LLOChannelIDAndDefinitionProto) (out llotypes.ChannelDefinitions, err error) {
	if len(in) > 0 {
		out = make(map[llotypes.ChannelID]llotypes.ChannelDefinition, len(in))
		for _, d := range in {
			if d.ChannelDefinition == nil {
				// Byzantine behavior makes this outcome invalid; a well-behaved
				// node should never encode nil definitions here
				return out, errors.New("failed to decode outcome; nil channel definition")
			}
			out[d.ChannelID] = protocol.ChannelDefinitionFromProto(d.ChannelDefinition)
		}
	}
	return out, nil
}

func streamAggregatesFromProtoOutcome(in []*protocol.LLOStreamAggregate) (out protocol.StreamAggregates, err error) {
	if len(in) > 0 {
		out = make(protocol.StreamAggregates, len(in))
		for _, enc := range in {
			var sv protocol.StreamValue
			sv, err = protocol.UnmarshalProtoStreamValue(enc.StreamValue)
			if err != nil {
				return
			}
			m, exists := out[enc.StreamID]
			if !exists {
				m = make(map[llotypes.Aggregator]protocol.StreamValue)
				out[enc.StreamID] = m
			}
			m[llotypes.Aggregator(enc.Aggregator)] = sv
		}
	}
	return
}
