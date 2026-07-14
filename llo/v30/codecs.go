package llo

import (
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

type ObservationCodec interface {
	Encode(obs Observation) (types.Observation, error)
	Decode(encoded types.Observation) (obs Observation, err error)
}

type OutcomeCodec interface {
	Encode(outcome Outcome) (ocr3types.Outcome, error)
	Decode(encoded ocr3types.Outcome) (outcome Outcome, err error)
}

// GetOutcomeCodec selects the outcome codec for the given offchain config's
// protocol version. It is a free function (rather than a method on the shared
// OffchainConfig type) because the outcome codecs are v3.0-specific.
func GetOutcomeCodec(c llocommon.OffchainConfig) OutcomeCodec {
	switch c.ProtocolVersion {
	case 0:
		return protoOutcomeCodecV0{}
	default:
		return protoOutcomeCodecV1{}
	}
}

// SelectBackfillCandidate returns the next observation timestamp in nanoseconds, its raw key, and parsed opts, or an UnreportableChannelError.
// outcome.ValidAfterNanoseconds[backfillCID] is the progress watermark (last emitted backfill observation time, in nanoseconds).
// Min report interval and second-resolution overlap rules used for live channels do not apply to history_backfill.
func SelectBackfillCandidate(out *Outcome, backfillCID llotypes.ChannelID) (tsNanos uint64, rawTS uint64, opts llocommon.HistoryBackfillOpts, uerr *UnreportableChannelError) {
	cd, exists := out.ChannelDefinitions[backfillCID]
	if !exists {
		return 0, 0, llocommon.HistoryBackfillOpts{}, &UnreportableChannelError{Inner: nil, Reason: "IsReportable=false; no channel definition with this ID", ChannelID: backfillCID}
	}
	if cd.ReportFormat != llotypes.ReportFormatHistoryBackfill {
		return 0, 0, llocommon.HistoryBackfillOpts{}, &UnreportableChannelError{Inner: fmt.Errorf("internal error: not a history_backfill channel"), Reason: "not history_backfill", ChannelID: backfillCID}
	}

	var err error
	opts, err = llocommon.ParseHistoryBackfillOpts(cd.Opts)
	if err != nil {
		return 0, 0, llocommon.HistoryBackfillOpts{}, &UnreportableChannelError{Inner: fmt.Errorf("failed to backfill: %w", err), Reason: "failed to parse history_backfill opts", ChannelID: backfillCID}
	}
	target, ok := out.ChannelDefinitions[opts.TargetChannelID]
	if !ok {
		return 0, 0, llocommon.HistoryBackfillOpts{}, &UnreportableChannelError{Inner: fmt.Errorf("failed to backfill: target channel %d not in outcome", opts.TargetChannelID), Reason: "missing target channel", ChannelID: backfillCID}
	}
	res, err := llocommon.TargetChannelTimeResolution(target)
	if err != nil {
		return 0, 0, llocommon.HistoryBackfillOpts{}, &UnreportableChannelError{Inner: fmt.Errorf("failed to backfill: %w", err), Reason: "invalid target channel time resolution opts", ChannelID: backfillCID}
	}
	watermark, ok := out.ValidAfterNanoseconds[backfillCID]
	if !ok {
		return 0, 0, llocommon.HistoryBackfillOpts{}, &UnreportableChannelError{Inner: nil, Reason: "IsReportable=false; no ValidAfterNanoseconds entry yet, this must be a new channel", ChannelID: backfillCID}
	}

	obsTSNanos := out.ObservationTimestampNanoseconds

	var bestRaw uint64
	var bestNanos uint64
	found := false
	for rawKey := range opts.Observations {
		tsN := llocommon.ObservationTimestampKeyToNanoseconds(rawKey, res)
		if tsN >= obsTSNanos {
			continue
		}
		if tsN <= watermark {
			continue
		}
		if !found || tsN < bestNanos || (tsN == bestNanos && rawKey < bestRaw) {
			found = true
			bestNanos = tsN
			bestRaw = rawKey
		}
	}

	if !found {
		return 0, 0, llocommon.HistoryBackfillOpts{}, &UnreportableChannelError{Inner: nil, Reason: "backfill complete, no remaining timestamps", ChannelID: backfillCID}
	}

	return bestNanos, bestRaw, opts, nil
}
