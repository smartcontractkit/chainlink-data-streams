package llo

import (
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

	observationsTimestampSeconds, err := outcome.ObservationsTimestampSeconds()
	if err != nil {
		return nil, fmt.Errorf("error getting observations timestamp: %w", err)
	}

	rwis := []ocr3types.ReportPlus[llotypes.ReportInfo]{}

	if outcome.LifeCycleStage == LifeCycleStageRetired {
		// if we're retired, emit special retirement report to transfer
		// ValidAfterSeconds part of state to the new protocol instance for a
		// "gapless" handover
		retirementReport := outcome.GenRetirementReport()
		p.Logger.Infow("Emitting retirement report", "lifeCycleStage", outcome.LifeCycleStage, "retirementReport", retirementReport, "stage", "Report", "seqNr", seqNr)

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

	reportableChannels, unreportableChannels := outcome.ReportableChannels()
	if p.Config.VerboseLogging {
		p.Logger.Debugw("Reportable channels", "lifeCycleStage", outcome.LifeCycleStage, "reportableChannels", reportableChannels, "unreportableChannels", unreportableChannels, "stage", "Report", "seqNr", seqNr)
	}

	for _, cid := range reportableChannels {
		cd := outcome.ChannelDefinitions[cid]
		values := make([]StreamValue, 0, len(cd.Streams))
		for _, strm := range cd.Streams {
			values = append(values, outcome.StreamAggregates[strm.StreamID][strm.Aggregator])
		}

		report := Report{
			p.ConfigDigest,
			seqNr,
			cid,
			outcome.ValidAfterSeconds[cid],
			observationsTimestampSeconds,
			values,
			outcome.LifeCycleStage != LifeCycleStageProduction,
		}

		if p.Config.VerboseLogging {
			p.Logger.Debugw("Emitting report", "lifeCycleStage", outcome.LifeCycleStage, "channelID", cid, "report", report, "stage", "Report", "seqNr", seqNr)
		}

		encoded, err := p.encodeReport(ctx, report, cd)
		if err != nil {
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
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

func (p *Plugin) encodeReport(ctx context.Context, r Report, cd llotypes.ChannelDefinition) (types.Report, error) {
	codec, exists := p.ReportCodecs[cd.ReportFormat]
	if !exists {
		return nil, fmt.Errorf("codec missing for ReportFormat=%q", cd.ReportFormat)
	}
	return codec.Encode(ctx, r, cd)
}
