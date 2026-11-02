package llo

import (
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
	// ChannelDefinitions are the definitions ChannelState encodes. The wire
	// carries ChannelState only, so a decoded precursor leaves this nil and
	// Reports resolves it through the channel cache (see definitions).
	ChannelDefinitions llotypes.ChannelDefinitions
	// ChannelState is the encoded c/defs record ChannelDefinitions were decoded
	// from. Nil encodes ChannelDefinitions instead, which yields the same bytes.
	ChannelState []byte
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

// encodePrecursor marshals and frames the precursor. libocr digests these
// bytes, so the framing must be deterministic, which encodeFramedPayload is.
func encodePrecursor(p precursor) (ocr3_1types.ReportsPlusPrecursor, error) {
	raw, err := marshalPrecursor(p)
	if err != nil {
		return nil, err
	}
	return framePrecursor(raw)
}

func framePrecursor(raw []byte) (ocr3_1types.ReportsPlusPrecursor, error) {
	return encodeFramedPayload(raw, maxDecompressedPrecursorBytes, ocr3_1types.MaxMaxReportsPlusPrecursorBytes, "precursor")
}

// marshalPrecursor serializes the precursor deterministically as
// LLOPrecursorProto, before framing.
func marshalPrecursor(p precursor) ([]byte, error) {
	pb := &protocol.LLOPrecursorProto{
		LifeCycleStage:                  string(p.LifeCycleStage),
		ObservationTimestampNanoseconds: p.ObservationTimestampNanoseconds,
		ChannelStateSeqNr:               p.ChannelStateSeqNr,
		ChannelState:                    p.ChannelState,
	}

	if pb.ChannelState == nil && len(p.ChannelDefinitions) > 0 {
		b, err := encodeChannelState(p.ChannelDefinitions)
		if err != nil {
			return nil, err
		}
		pb.ChannelState = b
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
			// A decoded precursor holds its definitions only encoded, and
			// definitions checks them when Reports resolves them.
			if _, ok := p.ChannelDefinitions[id]; !ok && p.ChannelDefinitions != nil {
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
// a peer that libocr verified against the certified digest and size limit. The
// channel definitions are left encoded in ChannelState: Reports resolves them
// through the channel cache, which in steady state already holds them. They are
// the committed set, so the per-definition stream and opts caps are not applied
// to them: they are admission-only, and rejecting a committed definition would
// fail Reports on every oracle.
func decodePrecursor(b ocr3_1types.ReportsPlusPrecursor) (precursor, error) {
	raw, err := decodeFramedPayload(b, maxDecompressedPrecursorBytes, "precursor")
	if err != nil {
		return precursor{}, err
	}
	pb := &protocol.LLOPrecursorProto{}
	if err := proto.Unmarshal(raw, pb); err != nil {
		return precursor{}, fmt.Errorf("unmarshal precursor: %w", err)
	}
	p := precursor{
		LifeCycleStage:                  llotypes.LifeCycleStage(pb.LifeCycleStage),
		ObservationTimestampNanoseconds: pb.ObservationTimestampNanoseconds,
		ChannelStateSeqNr:               pb.ChannelStateSeqNr,
		ChannelState:                    pb.ChannelState,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{},
		StreamAggregates:                protocol.StreamAggregates{},
	}
	for _, va := range pb.ValidAfterNanoseconds {
		p.ValidAfterNanoseconds[va.ChannelID] = va.ValidAfterNanoseconds
	}
	// Every reportable channel must appear once, in ascending order, so Reports
	// emits each at most once. definitions checks that each has a definition.
	// The collections are otherwise left unbounded on purpose: the precursor is
	// certified state, libocr bounds its size, and the promotion round
	// legitimately carries more validAfter entries than channels.
	if len(pb.ReportableChannelIDs) > 0 {
		p.ReportableChannelIDs = make([]llotypes.ChannelID, 0, len(pb.ReportableChannelIDs))
		for i, id := range pb.ReportableChannelIDs {
			if i > 0 && id <= pb.ReportableChannelIDs[i-1] {
				return precursor{}, fmt.Errorf("reportable channel IDs not strictly ascending at index %d", i)
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

// definitions resolves the precursor's channel definitions through cache,
// decoding ChannelState only on a miss, and sets ChannelDefinitions to them.
// The generation for ChannelStateSeqNr is a pure function of that sequence
// number, so a hit serves the same definitions ChannelState decodes to. The
// returned generation carries the opts decoded from exactly these definitions.
func (o *precursor) definitions(cache *protocol.ChannelCache) (*protocol.ChannelGeneration, error) {
	gen, err := cache.Load(o.ChannelStateSeqNr, func() ([]byte, error) { return o.ChannelState, nil }, decodeChannelState)
	if err != nil {
		return nil, fmt.Errorf("error loading channel generation: %w", err)
	}
	defs := gen.Definitions()
	for _, id := range o.ReportableChannelIDs {
		if _, ok := defs[id]; !ok {
			return nil, fmt.Errorf("reportable channel %d has no definition", id)
		}
	}
	o.ChannelDefinitions = defs
	return gen, nil
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
