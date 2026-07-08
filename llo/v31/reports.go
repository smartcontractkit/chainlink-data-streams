package llo

import (
	"context"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// Reports generates the (possibly empty) list of reports from a precursor. It
// receives no KeyValueStateReader, so the precursor must be fully self-sufficient.
func (p *Plugin) Reports(ctx context.Context, seqNr uint64, rawPrecursor ocr3_1types.ReportsPlusPrecursor) ([]ocr3types.ReportPlus[llotypes.ReportInfo], error) {
	if seqNr <= 1 {
		return nil, nil
	}

	out, err := decodePrecursor(rawPrecursor)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling precursor: %w", err)
	}

	rwis := []ocr3types.ReportPlus[llotypes.ReportInfo]{}

	if out.LifeCycleStage == llocommon.LifeCycleStageRetired {
		// Emit a retirement report to hand over ValidAfterNanoseconds for a gapless handover.
		retirementReport := llocommon.RetirementReport{
			ProtocolVersion:       p.ProtocolVersion,
			ValidAfterNanoseconds: out.ValidAfterNanoseconds,
		}
		encoded, err := p.RetirementReportCodec.Encode(retirementReport)
		if err != nil {
			return nil, fmt.Errorf("error encoding retirement report: %w", err)
		}
		rwis = append(rwis, ocr3types.ReportPlus[llotypes.ReportInfo]{
			ReportWithInfo: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
				Report: encoded,
				Info: llotypes.ReportInfo{
					LifeCycleStage: llocommon.LifeCycleStageRetired,
					ReportFormat:   llotypes.ReportFormatRetirement,
				},
			},
		})
	}

	for _, cid := range out.reportableChannels(p.DefaultMinReportIntervalNanoseconds) {
		cd := out.ChannelDefinitions[cid]

		if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			// TODO(v31-parity): history-backfill report emission.
			p.Logger.Warnw("history_backfill channels are not yet supported in v31; skipping", "channelID", cid, "stage", "Report", "seqNr", seqNr)
			continue
		}

		values := make([]llocommon.StreamValue, 0, len(cd.Streams))
		for _, strm := range cd.Streams {
			values = append(values, out.StreamAggregates[strm.StreamID][strm.Aggregator])
		}

		report := llocommon.Report{
			ConfigDigest:                    p.ConfigDigest,
			SeqNr:                           seqNr,
			ChannelID:                       cid,
			ValidAfterNanoseconds:           out.ValidAfterNanoseconds[cid],
			ObservationTimestampNanoseconds: out.ObservationTimestampNanoseconds,
			Values:                          values,
			Specimen:                        out.LifeCycleStage != llocommon.LifeCycleStageProduction,
		}

		p.captureReportTelemetry(report, cd)

		codec, exists := p.ReportCodecs[cd.ReportFormat]
		if !exists {
			p.Logger.Warnw("Error encoding report; codec missing for ReportFormat", "reportFormat", cd.ReportFormat, "channelID", cid, "stage", "Report", "seqNr", seqNr)
			continue
		}
		encoded, err := codec.Encode(report, cd, p.OptsCache)
		if err != nil {
			p.Logger.Warnw("Error encoding report", "reportFormat", cd.ReportFormat, "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
			continue
		}
		rwis = append(rwis, ocr3types.ReportPlus[llotypes.ReportInfo]{
			ReportWithInfo: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
				Report: encoded,
				Info: llotypes.ReportInfo{
					LifeCycleStage: out.LifeCycleStage,
					ReportFormat:   cd.ReportFormat,
				},
			},
		})
	}

	return rwis, nil
}

// reportableChannels returns the sorted set of channels reportable in this
// (current) round. Deferred: backfill, seconds-resolution overlap,
// DisableNilStreamValues (see doc.go).
func (o precursor) reportableChannels(minReportInterval uint64) []llotypes.ChannelID {
	reportable := make([]llotypes.ChannelID, 0, len(o.ChannelDefinitions))
	for channelID := range o.ChannelDefinitions {
		if o.isReportable(channelID, minReportInterval) {
			reportable = append(reportable, channelID)
		}
	}
	sort.Slice(reportable, func(i, j int) bool { return reportable[i] < reportable[j] })
	return reportable
}

func (o precursor) isReportable(channelID llotypes.ChannelID, minReportInterval uint64) bool {
	if o.LifeCycleStage == llocommon.LifeCycleStageRetired {
		return false
	}
	cd, exists := o.ChannelDefinitions[channelID]
	if !exists || cd.Tombstone {
		return false
	}
	if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
		return false // TODO(v31-parity)
	}
	// When DisableNilStreamValues is set, every stream must have a (non-nil)
	// aggregate value for the channel to be reportable.
	if cd.DisableNilStreamValues {
		for _, strm := range cd.Streams {
			if o.StreamAggregates[strm.StreamID][strm.Aggregator] == nil {
				return false
			}
		}
	}
	validAfter, ok := o.ValidAfterNanoseconds[channelID]
	if !ok {
		return false
	}
	if o.ObservationTimestampNanoseconds < validAfter+minReportInterval || o.ObservationTimestampNanoseconds <= validAfter {
		return false
	}
	// For seconds-resolution report formats, also require a full second between
	// validAfter and the observation timestamp to prevent overlapping reports.
	if isSecondsResolution(cd) && secondsOverlap(validAfter, o.ObservationTimestampNanoseconds) {
		return false
	}
	return true
}
