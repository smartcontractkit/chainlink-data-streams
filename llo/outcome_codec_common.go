package llo

import (
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// Common functions shared between outcome codecs

func StreamAggregatesToProtoOutcome(in StreamAggregates) (out []*LLOStreamAggregate, err error) {
	if len(in) > 0 {
		out = make([]*LLOStreamAggregate, 0, len(in))
		for sid, aggregates := range in {
			if aggregates == nil {
				return nil, fmt.Errorf("cannot marshal protobuf; nil value for stream ID: %d", sid)
			}
			for agg, v := range aggregates {
				if v == nil {
					return nil, fmt.Errorf("cannot marshal protobuf; nil value for stream ID: %d, aggregator: %v", sid, agg)
				}
				value, err := v.MarshalBinary()
				if err != nil {
					return nil, err
				}

				out = append(out, &LLOStreamAggregate{
					StreamID:    sid,
					StreamValue: &LLOStreamValue{Type: v.Type(), Value: value},
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

func channelDefinitionsToProtoOutcome(in llotypes.ChannelDefinitions) (out []*LLOChannelIDAndDefinitionProto) {
	if len(in) > 0 {
		out = make([]*LLOChannelIDAndDefinitionProto, 0, len(in))
		for id, d := range in {
			streams := make([]*LLOStreamDefinition, len(d.Streams))
			for i, strm := range d.Streams {
				streams[i] = &LLOStreamDefinition{
					StreamID:   strm.StreamID,
					Aggregator: uint32(strm.Aggregator),
				}
			}
			out = append(out, &LLOChannelIDAndDefinitionProto{
				ChannelID: id,
				ChannelDefinition: &LLOChannelDefinitionProto{
					ReportFormat: uint32(d.ReportFormat),
					Streams:      streams,
					Opts:         d.Opts,
				},
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].ChannelID < out[j].ChannelID
		})
	}
	return
}

func channelDefinitionsFromProtoOutcome(in []*LLOChannelIDAndDefinitionProto) (out llotypes.ChannelDefinitions, err error) {
	if len(in) > 0 {
		out = make(map[llotypes.ChannelID]llotypes.ChannelDefinition, len(in))
		for _, d := range in {
			if d.ChannelDefinition == nil {
				// Byzantine behavior makes this outcome invalid; a well-behaved
				// node should never encode nil definitions here
				return out, errors.New("failed to decode outcome; nil channel definition")
			}
			streams := make([]llotypes.Stream, len(d.ChannelDefinition.Streams))
			for i, strm := range d.ChannelDefinition.Streams {
				streams[i] = llotypes.Stream{
					StreamID:   strm.StreamID,
					Aggregator: llotypes.Aggregator(strm.Aggregator),
				}
			}
			out[d.ChannelID] = llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormat(d.ChannelDefinition.ReportFormat),
				Streams:      streams,
				Opts:         d.ChannelDefinition.Opts,
			}
		}
	}
	return out, nil
}

func streamAggregatesFromProtoOutcome(in []*LLOStreamAggregate) (out StreamAggregates, err error) {
	if len(in) > 0 {
		out = make(StreamAggregates, len(in))
		for _, enc := range in {
			var sv StreamValue
			sv, err = UnmarshalProtoStreamValue(enc.StreamValue)
			if err != nil {
				return
			}
			m, exists := out[enc.StreamID]
			if !exists {
				m = make(map[llotypes.Aggregator]StreamValue)
				out[enc.StreamID] = m
			}
			m[llotypes.Aggregator(enc.Aggregator)] = sv
		}
	}
	return
}
