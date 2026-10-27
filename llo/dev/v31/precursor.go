package llo

import (
	"errors"
	"fmt"
	"slices"
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
	// ReportableChannelIDs is the sorted set of channels StateTransition found
	// reportable this round (see isReportable). Reports emits exactly these, and
	// the next round advances validAfter from the same decision, so the two
	// cannot disagree.
	ReportableChannelIDs []llotypes.ChannelID
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

	if len(p.ReportableChannelIDs) > 0 {
		pb.ReportableChannelIDs = make([]uint32, 0, len(p.ReportableChannelIDs))
		for _, id := range p.ReportableChannelIDs {
			if _, ok := p.ChannelDefinitions[id]; !ok {
				return nil, fmt.Errorf("reportable channel %d has no definition", id)
			}
			pb.ReportableChannelIDs = append(pb.ReportableChannelIDs, id)
		}
		slices.Sort(pb.ReportableChannelIDs)
	}

	b, err := deterministicMarshal.Marshal(pb)
	if err != nil {
		return nil, fmt.Errorf("marshal precursor: %w", err)
	}
	return b, nil
}

// decodePrecursor decodes a precursor this oracle produced, or one fetched from
// a peer that libocr verified against the certified digest and size limit. Its
// channel definitions are the committed set, so the per-definition stream and
// opts caps are not applied here: they are admission-only, and rejecting a
// committed definition would fail Reports on every oracle.
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
	// Every reportable channel must have a definition and appear once, in
	// ascending order, so Reports emits each at most once. The collections are
	// otherwise left unbounded on purpose: the precursor is certified state,
	// libocr bounds its size, and the promotion round legitimately carries more
	// validAfter entries than channels.
	if len(pb.ReportableChannelIDs) > 0 {
		p.ReportableChannelIDs = make([]llotypes.ChannelID, 0, len(pb.ReportableChannelIDs))
		for i, id := range pb.ReportableChannelIDs {
			if i > 0 && id <= pb.ReportableChannelIDs[i-1] {
				return precursor{}, fmt.Errorf("reportable channel IDs not strictly ascending at index %d", i)
			}
			if _, ok := p.ChannelDefinitions[id]; !ok {
				return precursor{}, fmt.Errorf("reportable channel %d has no definition", id)
			}
			p.ReportableChannelIDs = append(p.ReportableChannelIDs, id)
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

// reportedAggregates returns the aggregates the reportable channels report,
// which is all Reports reads from StreamAggregates. Backfill reports carry
// their values in the backfill opts.
func (o precursor) reportedAggregates(optsCache *protocol.OptsCache) protocol.StreamAggregates {
	reported := protocol.StreamAggregates{}
	for _, cid := range o.ReportableChannelIDs {
		cd := o.ChannelDefinitions[cid]
		if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			continue
		}
		// isReportable derived the same streams, so this does not fail for a
		// reportable channel.
		streams, err := protocol.EffectiveStreams(optsCache, cd, cid)
		if err != nil {
			continue
		}
		for _, strm := range streams {
			if reported[strm.StreamID] == nil {
				reported[strm.StreamID] = map[llotypes.Aggregator]protocol.StreamValue{}
			}
			reported[strm.StreamID][strm.Aggregator] = o.StreamAggregates[strm.StreamID][strm.Aggregator]
		}
	}
	return reported
}
