package llo

import (
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"google.golang.org/protobuf/proto"
)

// precursor is the self-sufficient projection that StateTransition produces and
// Reports consumes. Reports receives no KeyValueStateReader, so everything it
// needs must be here. It is serialized deterministically as LLOPrecursorProto.
type precursor struct {
	LifeCycleStage                  llotypes.LifeCycleStage
	ObservationTimestampNanoseconds uint64
	ChannelDefinitions              llotypes.ChannelDefinitions
	ValidAfterNanoseconds           map[llotypes.ChannelID]uint64
	StreamAggregates                protocol.StreamAggregates
	// ChannelStateSeqNr is the c/seqnr of the channel-definitions record that
	// ChannelDefinitions came from. It lets Reports tell whether the decoded-opts
	// cache already matches these definitions without walking every channel.
	ChannelStateSeqNr uint64
}

func encodePrecursor(p precursor) (ocr3_1types.ReportsPlusPrecursor, error) {
	pb := &protocol.LLOPrecursorProto{
		LifeCycleStage:                  string(p.LifeCycleStage),
		ObservationTimestampNanoseconds: p.ObservationTimestampNanoseconds,
		ChannelStateSeqNr:               p.ChannelStateSeqNr,
	}

	if len(p.ChannelDefinitions) > 0 {
		pb.ChannelDefinitions = make([]*protocol.LLOChannelIDAndDefinitionProto, 0, len(p.ChannelDefinitions))
		for id, cd := range p.ChannelDefinitions {
			pb.ChannelDefinitions = append(pb.ChannelDefinitions, &protocol.LLOChannelIDAndDefinitionProto{
				ChannelID:         id,
				ChannelDefinition: protocol.ChannelDefinitionToProto(cd),
			})
		}
		sort.Slice(pb.ChannelDefinitions, func(i, j int) bool {
			return pb.ChannelDefinitions[i].ChannelID < pb.ChannelDefinitions[j].ChannelID
		})
	}

	if len(p.ValidAfterNanoseconds) > 0 {
		pb.ValidAfterNanoseconds = make([]*protocol.LLOChannelIDAndValidAfterNanosecondsProto, 0, len(p.ValidAfterNanoseconds))
		for id, va := range p.ValidAfterNanoseconds {
			pb.ValidAfterNanoseconds = append(pb.ValidAfterNanoseconds, &protocol.LLOChannelIDAndValidAfterNanosecondsProto{
				ChannelID:             id,
				ValidAfterNanoseconds: va,
			})
		}
		sort.Slice(pb.ValidAfterNanoseconds, func(i, j int) bool {
			return pb.ValidAfterNanoseconds[i].ChannelID < pb.ValidAfterNanoseconds[j].ChannelID
		})
	}

	if len(p.StreamAggregates) > 0 {
		for sid, aggregates := range p.StreamAggregates {
			for agg, v := range aggregates {
				pbSv, err := protocol.StreamValueToProto(v)
				if err != nil {
					return nil, fmt.Errorf("stream %d aggregator %v: %w", sid, agg, err)
				}
				pb.StreamAggregates = append(pb.StreamAggregates, &protocol.LLOStreamAggregate{
					StreamID:    sid,
					StreamValue: pbSv,
					Aggregator:  uint32(agg),
				})
			}
		}
		sort.Slice(pb.StreamAggregates, func(i, j int) bool {
			if pb.StreamAggregates[i].StreamID == pb.StreamAggregates[j].StreamID {
				return pb.StreamAggregates[i].Aggregator < pb.StreamAggregates[j].Aggregator
			}
			return pb.StreamAggregates[i].StreamID < pb.StreamAggregates[j].StreamID
		})
	}

	b, err := deterministicMarshal.Marshal(pb)
	if err != nil {
		return nil, fmt.Errorf("marshal precursor: %w", err)
	}
	return b, nil
}

func decodePrecursor(b ocr3_1types.ReportsPlusPrecursor) (precursor, error) {
	pb := &protocol.LLOPrecursorProto{}
	if err := proto.Unmarshal(b, pb); err != nil {
		return precursor{}, fmt.Errorf("unmarshal precursor: %w", err)
	}
	p := precursor{
		LifeCycleStage:                  llotypes.LifeCycleStage(pb.LifeCycleStage),
		ObservationTimestampNanoseconds: pb.ObservationTimestampNanoseconds,
		ChannelStateSeqNr:               pb.ChannelStateSeqNr,
		ChannelDefinitions:              llotypes.ChannelDefinitions{},
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{},
		StreamAggregates:                protocol.StreamAggregates{},
	}
	for _, cd := range pb.ChannelDefinitions {
		if cd.ChannelDefinition == nil {
			return precursor{}, errors.New("nil channel definition in precursor")
		}
		p.ChannelDefinitions[cd.ChannelID] = protocol.ChannelDefinitionFromProto(cd.ChannelDefinition)
	}
	for _, va := range pb.ValidAfterNanoseconds {
		p.ValidAfterNanoseconds[va.ChannelID] = va.ValidAfterNanoseconds
	}
	for _, sa := range pb.StreamAggregates {
		sv, err := protocol.UnmarshalProtoStreamValue(sa.StreamValue)
		if err != nil {
			return precursor{}, fmt.Errorf("stream %d: %w", sa.StreamID, err)
		}
		if p.StreamAggregates[sa.StreamID] == nil {
			p.StreamAggregates[sa.StreamID] = map[llotypes.Aggregator]protocol.StreamValue{}
		}
		p.StreamAggregates[sa.StreamID][llotypes.Aggregator(sa.Aggregator)] = sv
	}
	return p, nil
}
