package llo

import (
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
)

// Common functions shared between outcome codecs

func StreamAggregatesToProtoOutcome(in llocommon.StreamAggregates) (out []*llocommon.LLOStreamAggregate, err error) {
	if len(in) > 0 {
		out = make([]*llocommon.LLOStreamAggregate, 0, len(in))
		for sid, aggregates := range in {
			if aggregates == nil {
				return nil, fmt.Errorf("cannot marshal protobuf; nil aggregates for stream ID: %d", sid)
			}
			for agg, v := range aggregates {
				pbSv, err := llocommon.StreamValueToProto(v)
				if err != nil {
					return nil, fmt.Errorf("cannot marshal protobuf; stream ID: %d; aggregator: %v; %w", sid, agg, err)
				}
				out = append(out, &llocommon.LLOStreamAggregate{
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

func channelDefinitionsToProtoOutcome(in llotypes.ChannelDefinitions) (out []*llocommon.LLOChannelIDAndDefinitionProto) {
	if len(in) > 0 {
		out = make([]*llocommon.LLOChannelIDAndDefinitionProto, 0, len(in))
		for id, d := range in {
			out = append(out, &llocommon.LLOChannelIDAndDefinitionProto{
				ChannelID:         id,
				ChannelDefinition: llocommon.ChannelDefinitionToProto(d),
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].ChannelID < out[j].ChannelID
		})
	}
	return
}

func channelDefinitionsFromProtoOutcome(in []*llocommon.LLOChannelIDAndDefinitionProto) (out llotypes.ChannelDefinitions, err error) {
	if len(in) > 0 {
		out = make(map[llotypes.ChannelID]llotypes.ChannelDefinition, len(in))
		for _, d := range in {
			if d.ChannelDefinition == nil {
				// Byzantine behavior makes this outcome invalid; a well-behaved
				// node should never encode nil definitions here
				return out, errors.New("failed to decode outcome; nil channel definition")
			}
			out[d.ChannelID] = llocommon.ChannelDefinitionFromProto(d.ChannelDefinition)
		}
	}
	return out, nil
}

func streamAggregatesFromProtoOutcome(in []*llocommon.LLOStreamAggregate) (out llocommon.StreamAggregates, err error) {
	if len(in) > 0 {
		out = make(llocommon.StreamAggregates, len(in))
		for _, enc := range in {
			var sv llocommon.StreamValue
			sv, err = llocommon.UnmarshalProtoStreamValue(enc.StreamValue)
			if err != nil {
				return
			}
			m, exists := out[enc.StreamID]
			if !exists {
				m = make(map[llotypes.Aggregator]llocommon.StreamValue)
				out[enc.StreamID] = m
			}
			m[llotypes.Aggregator(enc.Aggregator)] = sv
		}
	}
	return
}
