package llo

import (
	"context"
	"fmt"
	"sort"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

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

	// Resolve the opts generation of the definitions this precursor was built
	// from, so the report codecs can never encode with opts that are ahead of (or
	// behind) the definitions being reported - not when a StateTransition for a
	// later seqNr is running concurrently, and not after a restart, where no
	// StateTransition ran on this node to populate the cache.
	gen, err := p.ChannelCache.Load(out.ChannelStateSeqNr, func() (llotypes.ChannelDefinitions, error) {
		return out.ChannelDefinitions, nil
	})
	if err != nil {
		return nil, fmt.Errorf("error loading channel generation: %w", err)
	}
	channelOpts := gen.Opts()

	rwis := []ocr3types.ReportPlus[llotypes.ReportInfo]{}

	if out.LifeCycleStage == protocol.LifeCycleStageRetired {
		// Emit a retirement report to hand over ValidAfterNanoseconds for a gapless handover.
		retirementReport := protocol.RetirementReport{
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
					LifeCycleStage: protocol.LifeCycleStageRetired,
					ReportFormat:   llotypes.ReportFormatRetirement,
				},
			},
		})
	}

	for _, cid := range out.reportableChannels(p.DefaultMinReportIntervalNanoseconds, channelOpts, p.Logger) {
		cd := out.ChannelDefinitions[cid]

		if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			tsNanos, rawTS, opts, ok := selectBackfillCandidate(out.ChannelDefinitions, out.ValidAfterNanoseconds, out.ObservationTimestampNanoseconds, cid, channelOpts)
			if !ok {
				p.Logger.Warnw("backfill channel was reportable but selection failed", "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			targetCD, exists := out.ChannelDefinitions[opts.TargetChannelID]
			if !exists {
				p.Logger.Warnw("missing target channel for history_backfill", "channelID", cid, "targetChannelID", opts.TargetChannelID, "stage", "Report", "seqNr", seqNr)
				continue
			}
			values, err := protocol.BuildBackfillStreamValues(targetCD, opts.Observations[rawTS])
			if err != nil {
				p.Logger.Warnw("Error building backfill stream values", "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			resNanos, err := protocol.ReportTimestampResolutionNanos(targetCD)
			if err != nil {
				p.Logger.Warnw("Error resolving history_backfill report timestamp resolution", "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			// The backfill report spans one resolution tick ending at the selected observation time.
			validAfter := uint64(0)
			if tsNanos >= resNanos {
				validAfter = tsNanos - resNanos
			}
			report := protocol.Report{
				ConfigDigest:                    p.ConfigDigest,
				SeqNr:                           seqNr,
				ChannelID:                       cid,
				ValidAfterNanoseconds:           validAfter,
				ObservationTimestampNanoseconds: tsNanos,
				Values:                          values,
				Specimen:                        out.LifeCycleStage != protocol.LifeCycleStageProduction,
			}
			// The report is encoded with, and attributed to, the target channel.
			reportForEncode := report
			reportForEncode.ChannelID = opts.TargetChannelID

			p.captureReportTelemetry(reportForEncode, targetCD)
			codec, exists := p.ReportCodecs[targetCD.ReportFormat]
			if !exists {
				p.Logger.Warnw("Error encoding backfill report; codec missing for target ReportFormat", "reportFormat", targetCD.ReportFormat, "channelID", cid, "targetChannelID", opts.TargetChannelID, "stage", "Report", "seqNr", seqNr)
				continue
			}
			encoded, err := codec.Encode(reportForEncode, targetCD, channelOpts)
			if err != nil {
				p.Logger.Warnw("Error encoding backfill report", "reportFormat", targetCD.ReportFormat, "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
				continue
			}
			rwis = append(rwis, ocr3types.ReportPlus[llotypes.ReportInfo]{
				ReportWithInfo: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
					Report: encoded,
					Info: llotypes.ReportInfo{
						LifeCycleStage: out.LifeCycleStage,
						ReportFormat:   targetCD.ReportFormat,
					},
				},
			})
			continue
		}

		// The streams a channel reports are derived, not read off the
		// definition: calculated streams live in the channel's opts and are
		// appended here, in declaration order, as the trailing values the codec
		// encodes as its payload.
		streams, err := protocol.EffectiveStreams(channelOpts, cd, cid)
		if err != nil {
			p.Logger.Warnw("Error encoding report; cannot derive effective streams", "err", err, "channelID", cid, "stage", "Report", "seqNr", seqNr)
			continue
		}
		values := make([]protocol.StreamValue, 0, len(streams))
		for _, strm := range streams {
			values = append(values, out.StreamAggregates[strm.StreamID][strm.Aggregator])
		}

		report := protocol.Report{
			ConfigDigest:                    p.ConfigDigest,
			SeqNr:                           seqNr,
			ChannelID:                       cid,
			ValidAfterNanoseconds:           out.ValidAfterNanoseconds[cid],
			ObservationTimestampNanoseconds: out.ObservationTimestampNanoseconds,
			Values:                          values,
			Specimen:                        out.LifeCycleStage != protocol.LifeCycleStageProduction,
		}

		p.captureReportTelemetry(report, cd)

		codec, exists := p.ReportCodecs[cd.ReportFormat]
		if !exists {
			p.Logger.Warnw("Error encoding report; codec missing for ReportFormat", "reportFormat", cd.ReportFormat, "channelID", cid, "stage", "Report", "seqNr", seqNr)
			continue
		}
		encoded, encErr := codec.Encode(report, cd, channelOpts)
		if encErr != nil {
			p.Logger.Warnw("Error encoding report", "reportFormat", cd.ReportFormat, "err", encErr, "channelID", cid, "stage", "Report", "seqNr", seqNr)
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
// (current) round (see isReportable).
func (o precursor) reportableChannels(minReportInterval uint64, optsCache *protocol.OptsCache, lggr logger.Logger) []llotypes.ChannelID {
	reportable := make([]llotypes.ChannelID, 0, len(o.ChannelDefinitions))
	for channelID := range o.ChannelDefinitions {
		if o.isReportable(channelID, minReportInterval, optsCache, lggr) {
			reportable = append(reportable, channelID)
		}
	}
	sort.Slice(reportable, func(i, j int) bool { return reportable[i] < reportable[j] })
	return reportable
}

func (o precursor) isReportable(channelID llotypes.ChannelID, minReportInterval uint64, optsCache *protocol.OptsCache, lggr logger.Logger) bool {
	if o.LifeCycleStage == protocol.LifeCycleStageRetired {
		return false
	}
	cd, exists := o.ChannelDefinitions[channelID]
	if !exists || cd.Tombstone {
		return false
	}
	if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
		_, _, _, ok := selectBackfillCandidate(o.ChannelDefinitions, o.ValidAfterNanoseconds, o.ObservationTimestampNanoseconds, channelID, optsCache)
		return ok
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
	// Calculated streams are derived state, and unlike observed streams a
	// missing one cannot be reported around: the codec has nothing to encode, so
	// Reports skips the report. Without this check the channel would still be
	// counted as reported and validAfter would advance over a round that emitted
	// nothing — a silent coverage gap. This is independent of
	// DisableNilStreamValues, which is about observed values.
	//
	// The check is against the streams the channel's opts declare, which is also
	// what protocol.EffectiveStreams derives the report's trailing values from.
	// A channel whose expressions failed (bad input, undecodable opts, eval
	// error, or history still warming up) has no aggregate for them.
	if protocol.HasCalculatedStreams(cd) {
		calculatedStreamIDs, err := protocol.CalculatedStreamIDs(optsCache, cd, channelID)
		if err != nil {
			lggr.Warnw("IsReportable=false; cannot resolve calculated stream IDs", "channelID", channelID, "err", err)
			return false
		}
		for _, sid := range calculatedStreamIDs {
			if o.StreamAggregates[sid][llotypes.AggregatorCalculated] == nil {
				lggr.Warnw("IsReportable=false; nil calculated stream value", "channelID", channelID, "streamID", sid)
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
