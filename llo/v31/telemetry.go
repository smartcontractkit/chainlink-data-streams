package llo

import (
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// Telemetry is emitted best-effort on buffered channels; a full channel drops
// the datum rather than blocking the protocol. Emission is an unobservable side
// effect and does not affect StateTransition/Reports return values or state.

func (p *Plugin) captureOutcomeTelemetry(out precursor, seqNr uint64) {
	if p.OutcomeTelemetryCh == nil {
		return
	}
	ot, err := makeOutcomeTelemetry(out, p.ConfigDigest, seqNr, p.DonID)
	if err != nil {
		p.Logger.Warnw("Error making outcome telemetry", "err", err)
		return
	}
	select {
	case p.OutcomeTelemetryCh <- ot:
	default:
		p.Logger.Warn("OutcomeTelemetryCh is full, dropping telemetry")
	}
}

func makeOutcomeTelemetry(out precursor, configDigest ocrtypes.ConfigDigest, seqNr uint64, donID uint32) (*llocommon.LLOOutcomeTelemetry, error) {
	ot := &llocommon.LLOOutcomeTelemetry{
		LifeCycleStage:                  string(out.LifeCycleStage),
		ObservationTimestampNanoseconds: out.ObservationTimestampNanoseconds,
		ChannelDefinitions:              make(map[uint32]*llocommon.LLOChannelDefinitionProto, len(out.ChannelDefinitions)),
		ValidAfterNanoseconds:           make(map[uint32]uint64, len(out.ValidAfterNanoseconds)),
		StreamAggregates:                make(map[uint32]*llocommon.LLOAggregatorStreamValue, len(out.StreamAggregates)),
		SeqNr:                           seqNr,
		ConfigDigest:                    configDigest[:],
		DonId:                           donID,
	}
	for id, cd := range out.ChannelDefinitions {
		ot.ChannelDefinitions[id] = llocommon.ChannelDefinitionToProto(cd)
	}
	for id, va := range out.ValidAfterNanoseconds {
		ot.ValidAfterNanoseconds[id] = va
	}
	for sid, aggMap := range out.StreamAggregates {
		if len(aggMap) == 0 {
			continue
		}
		aggVals := make(map[uint32]*llocommon.LLOStreamValue, len(aggMap))
		for agg, sv := range aggMap {
			v, err := llocommon.StreamValueToProto(sv)
			if err != nil {
				return nil, fmt.Errorf("failed to make outcome telemetry; %w", err)
			}
			aggVals[uint32(agg)] = v
		}
		ot.StreamAggregates[sid] = &llocommon.LLOAggregatorStreamValue{AggregatorValues: aggVals}
	}
	return ot, nil
}

func (p *Plugin) captureReportTelemetry(r llocommon.Report, cd llotypes.ChannelDefinition) {
	if p.ReportTelemetryCh == nil {
		return
	}
	rt, err := makeReportTelemetry(r, cd, p.DonID)
	if err != nil {
		p.Logger.Warnw("Error making report telemetry", "err", err)
		return
	}
	select {
	case p.ReportTelemetryCh <- rt:
	default:
		p.Logger.Warn("ReportTelemetryCh is full, dropping telemetry")
	}
}

func makeReportTelemetry(r llocommon.Report, cd llotypes.ChannelDefinition, donID uint32) (*llocommon.LLOReportTelemetry, error) {
	streams := make([]*llocommon.LLOStreamDefinition, len(cd.Streams))
	for i, s := range cd.Streams {
		streams[i] = &llocommon.LLOStreamDefinition{
			StreamID:   s.StreamID,
			Aggregator: uint32(s.Aggregator),
		}
	}
	svs := make([]*llocommon.LLOStreamValue, len(r.Values))
	for i, v := range r.Values {
		if v == nil {
			// Missing stream value (allowed when DisableNilStreamValues is false);
			// emit an empty entry rather than panicking.
			svs[i] = &llocommon.LLOStreamValue{}
			continue
		}
		b, err := v.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("error marshalling stream value: %w", err)
		}
		svs[i] = &llocommon.LLOStreamValue{
			Type:  v.Type(),
			Value: b,
		}
	}
	rt := &llocommon.LLOReportTelemetry{
		ChannelId:                       r.ChannelID,
		ValidAfterNanoseconds:           r.ValidAfterNanoseconds,
		ObservationTimestampNanoseconds: r.ObservationTimestampNanoseconds,
		ReportFormat:                    uint32(cd.ReportFormat),
		Specimen:                        r.Specimen,
		StreamDefinitions:               streams,
		StreamValues:                    svs,
		ChannelOpts:                     cd.Opts,
		SeqNr:                           r.SeqNr,
		ConfigDigest:                    r.ConfigDigest[:],
		DonId:                           donID,
	}
	return rt, nil
}
