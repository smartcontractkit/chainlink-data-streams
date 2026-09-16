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
	// SupportByFormat is the number of oracles that advertised a report codec
	// for each report format in the round that produced this precursor.
	//
	// Snapshotted rather than recomputed so that reportability and the
	// validAfter advance for a round read the identical number: Reports runs
	// against this precursor, and StateTransition persists reportedLastRound
	// from the same object (see isReportable).
	SupportByFormat map[llotypes.ReportFormat]int
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

	if len(p.SupportByFormat) > 0 {
		pb.SupportByFormat = make([]*protocol.LLOReportFormatSupportProto, 0, len(p.SupportByFormat))
		for format, count := range p.SupportByFormat {
			if count < 0 {
				return nil, fmt.Errorf("negative support count for report format %v: %d", format, count)
			}
			pb.SupportByFormat = append(pb.SupportByFormat, &protocol.LLOReportFormatSupportProto{
				ReportFormat: uint32(format),
				OracleCount:  uint32(count),
			})
		}
		sort.Slice(pb.SupportByFormat, func(i, j int) bool {
			return pb.SupportByFormat[i].ReportFormat < pb.SupportByFormat[j].ReportFormat
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
	if len(pb.SupportByFormat) > protocol.MaxObservationSupportedReportFormatsLength {
		return precursor{}, fmt.Errorf("precursor carries too many report format support entries: %d (max %d)", len(pb.SupportByFormat), protocol.MaxObservationSupportedReportFormatsLength)
	}
	if len(pb.SupportByFormat) > 0 {
		p.SupportByFormat = make(map[llotypes.ReportFormat]int, len(pb.SupportByFormat))
		for _, sup := range pb.SupportByFormat {
			p.SupportByFormat[llotypes.ReportFormat(sup.ReportFormat)] = int(sup.OracleCount)
		}
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
