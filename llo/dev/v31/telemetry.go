package llo

import (
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

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

func makeOutcomeTelemetry(out precursor, configDigest ocrtypes.ConfigDigest, seqNr uint64, donID uint32) (*protocol.LLOOutcomeTelemetry, error) {
	ot := &protocol.LLOOutcomeTelemetry{
		LifeCycleStage:                  string(out.LifeCycleStage),
		ObservationTimestampNanoseconds: out.ObservationTimestampNanoseconds,
		ChannelDefinitions:              make(map[uint32]*protocol.LLOChannelDefinitionProto, len(out.ChannelDefinitions)),
		ValidAfterNanoseconds:           make(map[uint32]uint64, len(out.ValidAfterNanoseconds)),
		StreamAggregates:                make(map[uint32]*protocol.LLOAggregatorStreamValue, len(out.StreamAggregates)),
		SeqNr:                           seqNr,
		ConfigDigest:                    configDigest[:],
		DonId:                           donID,
	}
	for id, cd := range out.ChannelDefinitions {
		ot.ChannelDefinitions[id] = protocol.ChannelDefinitionToProto(cd)
	}
	for id, va := range out.ValidAfterNanoseconds {
		ot.ValidAfterNanoseconds[id] = va
	}
	for sid, aggMap := range out.StreamAggregates {
		if len(aggMap) == 0 {
			continue
		}
		aggVals := make(map[uint32]*protocol.LLOStreamValue, len(aggMap))
		for agg, sv := range aggMap {
			v, err := protocol.StreamValueToProto(sv)
			if err != nil {
				return nil, fmt.Errorf("failed to make outcome telemetry; %w", err)
			}
			aggVals[uint32(agg)] = v
		}
		ot.StreamAggregates[sid] = &protocol.LLOAggregatorStreamValue{AggregatorValues: aggVals}
	}
	return ot, nil
}

func (p *Plugin) captureReportTelemetry(r protocol.Report, cd llotypes.ChannelDefinition) {
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

func makeReportTelemetry(r protocol.Report, cd llotypes.ChannelDefinition, donID uint32) (*protocol.LLOReportTelemetry, error) {
	streams := make([]*protocol.LLOStreamDefinition, len(cd.Streams))
	for i, s := range cd.Streams {
		streams[i] = &protocol.LLOStreamDefinition{
			StreamID:   s.StreamID,
			Aggregator: uint32(s.Aggregator),
		}
	}
	svs := make([]*protocol.LLOStreamValue, len(r.Values))
	for i, v := range r.Values {
		if v == nil {
			// Missing stream value (allowed when DisableNilStreamValues is false);
			// emit an empty entry rather than panicking.
			svs[i] = &protocol.LLOStreamValue{}
			continue
		}
		b, err := v.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("error marshalling stream value: %w", err)
		}
		svs[i] = &protocol.LLOStreamValue{
			Type:  v.Type(),
			Value: b,
		}
	}
	rt := &protocol.LLOReportTelemetry{
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
