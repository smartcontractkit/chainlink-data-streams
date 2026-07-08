package llo

import (
	. "github.com/smartcontractkit/chainlink-data-streams/llo"

	"context"
	"fmt"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func (p *Plugin) reports(ctx context.Context, seqNr uint64, rawOutcome ocr3types.Outcome) ([]ocr3types.ReportPlus[llotypes.ReportInfo], error) {
	if seqNr <= 1 {
		// no reports for initial round
		return nil, nil
	}

	outcome, err := p.OutcomeCodec.Decode(rawOutcome)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling outcome: %w", err)
	}

	rwis := []ocr3types.ReportPlus[llotypes.ReportInfo]{}

	if outcome.LifeCycleStage == LifeCycleStageRetired {
		// if we're retired, emit special retirement report to transfer
		// ValidAfterNanoseconds part of state to the new protocol instance for a
		// "gapless" handover
		retirementReport := outcome.GenRetirementReport(p.ProtocolVersion)
		p.Logger.Debugw("Emitting retirement report", "lifeCycleStage", outcome.LifeCycleStage, "retirementReport", retirementReport, "stage", "Report", "seqNr", seqNr)

		encoded, err := p.RetirementReportCodec.Encode(retirementReport)
		if err != nil {
			return nil, fmt.Errorf("error encoding retirement report: %w", err)
		}

		rwis = append(rwis, ocr3types.ReportPlus[llotypes.ReportInfo]{
			ReportWithInfo: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
				Report: encoded,
				Info: llotypes.ReportInfo{
					LifeCycleStage: LifeCycleStageRetired,
					ReportFormat:   llotypes.ReportFormatRetirement,
				},
			},
		})
	}

	reportableChannels, unreportableChannels := outcome.ReportableChannels(p.ProtocolVersion, p.DefaultMinReportIntervalNanoseconds, p.OptsCache)
	if p.Config.VerboseLogging {
		p.Logger.Debugw("Reportable channels", "lifeCycleStage", outcome.LifeCycleStage, "reportableChannels", reportableChannels, "unreportableChannels", unreportableChannels, "stage", "Report", "seqNr", seqNr)
	}

	for _, cid := range reportableChannels {
		cd := outcome.ChannelDefinitions[cid]

		if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			tsNanos, rawTS, opts, uerr := SelectBackfillCandidate(&outcome, cid)
			if uerr != nil {
				p.Logger.Warnw("backfill channel was reportable but selection failed", "channelID", cid, "err", uerr, "stage", "Report", "seqNr", seqNr)
				continue
			}
			targetCD, ok := outcome.ChannelDefinitions[opts.TargetChannelID]
			if !ok {
				p.Logger.Warnw("missing target channel for history_backfill", "channelID", cid, "targetChannelID", opts.TargetChannelID, "stage", "Report", "seqNr", seqNr)
				continue
			}
			row := opts.Observations[rawTS]
			values, err := BuildBackfillStreamValues(targetCD, row)
			if err != nil {
				p.Logger.Warnw("Error building backfill stream values", "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			resNanos, err := ReportTimestampResolutionNanos(targetCD)
			if err != nil {
				p.Logger.Warnw("Error resolving history_backfill report timestamp resolution", "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			validAfter := uint64(0)
			// Guard against poorly formatted timestamps being passed from the backfill channel.
			if tsNanos >= resNanos {
				validAfter = tsNanos - resNanos
			}
			report := Report{
				ConfigDigest:                    p.ConfigDigest,
				SeqNr:                           seqNr,
				ChannelID:                       cid,
				ValidAfterNanoseconds:           validAfter,
				ObservationTimestampNanoseconds: tsNanos,
				Values:                          values,
				Specimen:                        outcome.LifeCycleStage != LifeCycleStageProduction,
			}
			reportForEncode := report
			reportForEncode.ChannelID = opts.TargetChannelID

			if p.Config.VerboseLogging {
				p.Logger.Debugw("Emitting history_backfill report", "lifeCycleStage", outcome.LifeCycleStage, "channelID", cid, "targetChannelID", opts.TargetChannelID, "report", report, "stage", "Report", "seqNr", seqNr)
			}

			p.captureReportTelemetry(reportForEncode, targetCD)
			codec, exists := p.ReportCodecs[targetCD.ReportFormat]
			if !exists {
				p.Logger.Warnw("Error encoding report", "lifeCycleStage", outcome.LifeCycleStage, "reportFormat", targetCD.ReportFormat, "err", fmt.Errorf("codec missing for ReportFormat=%q", targetCD.ReportFormat), "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			encoded, err := codec.Encode(reportForEncode, targetCD, p.OptsCache)
			if err != nil {
				p.Logger.Warnw("Error encoding report", "lifeCycleStage", outcome.LifeCycleStage, "reportFormat", targetCD.ReportFormat, "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			rwis = append(rwis, ocr3types.ReportPlus[llotypes.ReportInfo]{
				ReportWithInfo: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
					Report: encoded,
					Info: llotypes.ReportInfo{
						LifeCycleStage: outcome.LifeCycleStage,
						ReportFormat:   targetCD.ReportFormat,
					},
				},
			})
			continue
		}

		values := make([]StreamValue, 0, len(cd.Streams))
		for _, strm := range cd.Streams {
			values = append(values, outcome.StreamAggregates[strm.StreamID][strm.Aggregator])
		}

		report := Report{
			ConfigDigest:                    p.ConfigDigest,
			SeqNr:                           seqNr,
			ChannelID:                       cid,
			ValidAfterNanoseconds:           outcome.ValidAfterNanoseconds[cid],
			ObservationTimestampNanoseconds: outcome.ObservationTimestampNanoseconds,
			Values:                          values,
			Specimen:                        outcome.LifeCycleStage != LifeCycleStageProduction,
		}

		if p.Config.VerboseLogging {
			p.Logger.Debugw("Emitting report", "lifeCycleStage", outcome.LifeCycleStage, "channelID", cid, "report", report, "stage", "Report", "seqNr", seqNr)
		}

		encoded, err := p.encodeReport(report, cd)
		if err != nil {
			p.Logger.Warnw("Error encoding report", "lifeCycleStage", outcome.LifeCycleStage, "reportFormat", cd.ReportFormat, "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
			continue
		}
		rwis = append(rwis, ocr3types.ReportPlus[llotypes.ReportInfo]{
			ReportWithInfo: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
				Report: encoded,
				Info: llotypes.ReportInfo{
					LifeCycleStage: outcome.LifeCycleStage,
					ReportFormat:   cd.ReportFormat,
				},
			},
		})
	}

	if p.Config.VerboseLogging && len(rwis) == 0 {
		p.Logger.Debugw("No reports, will not transmit anything", "lifeCycleStage", outcome.LifeCycleStage, "reportableChannels", reportableChannels, "stage", "Report", "seqNr", seqNr)
	}

	return rwis, nil
}

func (p *Plugin) encodeReport(r Report, cd llotypes.ChannelDefinition) (types.Report, error) {
	codec, exists := p.ReportCodecs[cd.ReportFormat]
	if !exists {
		return nil, fmt.Errorf("codec missing for ReportFormat=%q", cd.ReportFormat)
	}
	p.captureReportTelemetry(r, cd)
	return codec.Encode(r, cd, p.OptsCache)
}

func (p *Plugin) captureReportTelemetry(r Report, cd llotypes.ChannelDefinition) {
	if p.ReportTelemetryCh != nil {
		rt, err := makeReportTelemetry(r, cd, p.DonID)
		if err != nil {
			p.Logger.Warnw("Error making report telemetry", "err", err)
		} else {
			select {
			case p.ReportTelemetryCh <- rt:
			default:
				p.Logger.Warn("ReportTelemetryCh is full, dropping telemetry")
			}
		}
	}
}

func makeReportTelemetry(r Report, cd llotypes.ChannelDefinition, donID uint32) (*LLOReportTelemetry, error) {
	streams := make([]*LLOStreamDefinition, len(cd.Streams))
	for i, s := range cd.Streams {
		streams[i] = &LLOStreamDefinition{
			StreamID:   s.StreamID,
			Aggregator: uint32(s.Aggregator),
		}
	}
	svs := make([]*LLOStreamValue, len(r.Values))
	for i, v := range r.Values {
		b, err := v.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("error marshalling stream value: %w", err)
		}
		svs[i] = &LLOStreamValue{
			Type:  v.Type(),
			Value: b,
		}
	}
	rt := &LLOReportTelemetry{
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
