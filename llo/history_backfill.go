package llo

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/goccy/go-json"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// HistoryBackfillOpts is the canonical JSON shape for history_backfill channel opts
// (after any DON-specific flattening into ChannelDefinition.Opts).
type HistoryBackfillOpts struct {
	TargetChannelID llotypes.ChannelID `json:"targetChannelId"`
	// Observations maps raw timestamp keys (in the target channel's time resolution)
	// to stream ID -> serialized stream value string.
	Observations map[uint64]map[llotypes.StreamID]string `json:"-"`
}

type historyBackfillOptsWire struct {
	TargetChannelID llotypes.ChannelID           `json:"targetChannelId"`
	Observations    map[string]map[string]string `json:"observations"`
}

// UnmarshalJSON decodes observations with string keys into uint64 maps.
func (o *HistoryBackfillOpts) UnmarshalJSON(data []byte) error {
	var w historyBackfillOptsWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	o.TargetChannelID = w.TargetChannelID
	if len(w.Observations) == 0 {
		return errors.New("observations must be non-empty")
	}
	o.Observations = make(map[uint64]map[llotypes.StreamID]string, len(w.Observations))
	for tsStr, streams := range w.Observations {
		ts, err := strconv.ParseUint(tsStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid observation timestamp key %q: %w", tsStr, err)
		}
		if len(streams) == 0 {
			return fmt.Errorf("empty stream map for timestamp %s", tsStr)
		}
		inner := make(map[llotypes.StreamID]string, len(streams))
		for sidStr, val := range streams {
			sid64, err := strconv.ParseUint(sidStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid stream id key %q at timestamp %s: %w", sidStr, tsStr, err)
			}
			inner[llotypes.StreamID(sid64)] = val
		}
		o.Observations[ts] = inner
	}
	return nil
}

// ParseHistoryBackfillOpts decodes opts bytes into HistoryBackfillOpts.
func ParseHistoryBackfillOpts(raw llotypes.ChannelOpts) (HistoryBackfillOpts, error) {
	var o HistoryBackfillOpts
	if len(raw) == 0 {
		return o, errors.New("empty opts")
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, err
	}
	if o.TargetChannelID == 0 {
		return o, errors.New("targetChannelId must be non-zero")
	}
	if len(o.Observations) == 0 {
		return o, errors.New("observations must be non-empty")
	}
	return o, nil
}

// ReportCodecHistoryBackfill validates channel definitions; encoding is delegated to the target channel codec.
type ReportCodecHistoryBackfill struct{}

func (ReportCodecHistoryBackfill) Encode(Report, llotypes.ChannelDefinition, *OptsCache) ([]byte, error) {
	return nil, errors.New("history_backfill channel reports must be encoded with the target channel's report codec")
}

func (ReportCodecHistoryBackfill) Verify(cd llotypes.ChannelDefinition) error {
	_, err := ParseHistoryBackfillOpts(cd.Opts)
	return err
}

// ReportTimestampResolutionNanos returns one tick of the target channel's observation timestamp resolution in nanoseconds.
func ReportTimestampResolutionNanos(target llotypes.ChannelDefinition) (uint64, error) {
	switch target.ReportFormat {
	case llotypes.ReportFormatEVMPremiumLegacy:
		return 1e9, nil
	case llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.ReportFormatEVMABIEncodeUnpackedExpr:
		res, err := targetChannelTimeResolution(target)
		if err != nil {
			return 0, err
		}
		switch res {
		case ResolutionMilliseconds:
			return 1e6, nil
		case ResolutionMicroseconds:
			return 1e3, nil
		case ResolutionNanoseconds:
			return 1, nil
		default:
			return 1e9, nil
		}
	default:
		return 1e9, nil
	}
}

// ObservationTimestampKeyToNanoseconds converts a raw observation timestamp key from opts to nanoseconds.
func ObservationTimestampKeyToNanoseconds(rawKey uint64, res TimeResolution) uint64 {
	switch res {
	case ResolutionSeconds:
		return rawKey * 1e9
	case ResolutionMilliseconds:
		return rawKey * 1e6
	case ResolutionMicroseconds:
		return rawKey * 1e3
	case ResolutionNanoseconds:
		return rawKey
	default:
		return rawKey * 1e9
	}
}

func targetChannelTimeResolution(target llotypes.ChannelDefinition) (TimeResolution, error) {
	switch target.ReportFormat {
	case llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.ReportFormatEVMABIEncodeUnpackedExpr:
		if len(target.Opts) == 0 {
			return ResolutionSeconds, nil
		}
		var aux struct {
			TimeResolution TimeResolution `json:"TimeResolution"`
		}
		if err := json.Unmarshal(target.Opts, &aux); err != nil {
			return 0, fmt.Errorf("target channel opts: %w", err)
		}
		return aux.TimeResolution, nil
	default:
		return ResolutionSeconds, nil
	}
}

// ValidateHistoryBackfillAgainstDefinitions checks a single history_backfill definition against the full map.
// If nowNanos > 0, observation timestamps (converted to nanoseconds) must be < nowNanos.
func ValidateHistoryBackfillAgainstDefinitions(cd llotypes.ChannelDefinition, defs llotypes.ChannelDefinitions, nowNanos uint64) error {
	opts, err := ParseHistoryBackfillOpts(cd.Opts)
	if err != nil {
		return err
	}
	target, ok := defs[opts.TargetChannelID]
	if !ok {
		return fmt.Errorf("target channel %d not found", opts.TargetChannelID)
	}
	if len(cd.Streams) != len(target.Streams) {
		return fmt.Errorf("backfill streams must match target: got %d want %d", len(cd.Streams), len(target.Streams))
	}
	for i := range cd.Streams {
		if cd.Streams[i] != target.Streams[i] {
			return fmt.Errorf("backfill stream %d differs from target", i)
		}
	}
	for _, strm := range target.Streams {
		if strm.Aggregator == llotypes.AggregatorCalculated {
			return errors.New("history backfill target channel must not use calculated streams (phase 1 limitation)")
		}
	}
	res, err := targetChannelTimeResolution(target)
	if err != nil {
		return err
	}
	for rawTS, streams := range opts.Observations {
		tsNanos := ObservationTimestampKeyToNanoseconds(rawTS, res)
		if nowNanos > 0 && tsNanos >= nowNanos {
			return fmt.Errorf("observation timestamp %d (raw %d) is not strictly in the past relative to reference time", tsNanos, rawTS)
		}
		if len(streams) != len(target.Streams) {
			return fmt.Errorf("timestamp %d: expected %d stream values, got %d", rawTS, len(target.Streams), len(streams))
		}
		for _, strm := range target.Streams {
			if _, ok := streams[strm.StreamID]; !ok {
				return fmt.Errorf("timestamp %d: missing stream %d", rawTS, strm.StreamID)
			}
		}
	}
	return nil
}

// DropInvalidHistoryBackfillChannels returns a copy of defs without history_backfill channels that fail validation.
// The input defs map is not modified. nowNanos should be wall-clock nanoseconds; use 0 to skip the future-timestamp check.
func DropInvalidHistoryBackfillChannels(lggr logger.Logger, defs llotypes.ChannelDefinitions, nowNanos uint64) llotypes.ChannelDefinitions {
	if defs == nil {
		return nil
	}
	out := make(llotypes.ChannelDefinitions, len(defs))
	for k, v := range defs {
		out[k] = v
	}
	for cid, cd := range out {
		if cd.ReportFormat != llotypes.ReportFormatHistoryBackfill {
			continue
		}
		if err := ValidateHistoryBackfillAgainstDefinitions(cd, out, nowNanos); err != nil {
			logger.Sugared(lggr).Warnw("dropping invalid history_backfill channel definition", "channelID", cid, "err", err)
			delete(out, cid)
		}
	}
	return out
}

// StreamValueFromBackfillString parses a backfill observation string for the given aggregator.
func StreamValueFromBackfillString(agg llotypes.Aggregator, s string) (StreamValue, error) {
	switch agg {
	case llotypes.AggregatorQuote:
		q := new(Quote) // {Bid: ([0-9.]+), Benchmark: ([0-9.]+), Ask: ([0-9.]+)}
		if err := q.UnmarshalText([]byte(s)); err != nil {
			return nil, err
		}
		return q, nil
	default:
		d := new(Decimal)
		if err := d.UnmarshalText([]byte(s)); err != nil {
			return nil, err
		}
		return d, nil
	}
}

// BuildBackfillStreamValues builds stream values for a backfill timestamp row in target stream order.
func BuildBackfillStreamValues(target llotypes.ChannelDefinition, row map[llotypes.StreamID]string) ([]StreamValue, error) {
	values := make([]StreamValue, 0, len(target.Streams))
	for _, strm := range target.Streams {
		s, ok := row[strm.StreamID]
		if !ok {
			return nil, fmt.Errorf("missing stream %d", strm.StreamID)
		}
		sv, err := StreamValueFromBackfillString(strm.Aggregator, s)
		if err != nil {
			return nil, fmt.Errorf("stream %d: %w", strm.StreamID, err)
		}
		values = append(values, sv)
	}
	return values, nil
}

// SelectBackfillCandidate returns the next observation timestamp in nanoseconds, its raw key, and parsed opts, or an UnreportableChannelError.
// outcome.ValidAfterNanoseconds[backfillCID] is the progress watermark (last emitted backfill observation time, in nanoseconds).
// Min report interval and second-resolution overlap rules used for live channels do not apply to history_backfill.
func SelectBackfillCandidate(out *Outcome, backfillCID llotypes.ChannelID) (tsNanos uint64, rawTS uint64, opts HistoryBackfillOpts, uerr *UnreportableChannelError) {
	cd, exists := out.ChannelDefinitions[backfillCID]
	if !exists {
		return 0, 0, HistoryBackfillOpts{}, &UnreportableChannelError{nil, "IsReportable=false; no channel definition with this ID", backfillCID}
	}
	if cd.ReportFormat != llotypes.ReportFormatHistoryBackfill {
		return 0, 0, HistoryBackfillOpts{}, &UnreportableChannelError{fmt.Errorf("internal error: not a history_backfill channel"), "not history_backfill", backfillCID}
	}

	var err error
	opts, err = ParseHistoryBackfillOpts(cd.Opts)
	if err != nil {
		return 0, 0, HistoryBackfillOpts{}, &UnreportableChannelError{fmt.Errorf("failed to backfill: %w", err), "failed to parse history_backfill opts", backfillCID}
	}
	target, ok := out.ChannelDefinitions[opts.TargetChannelID]
	if !ok {
		return 0, 0, HistoryBackfillOpts{}, &UnreportableChannelError{fmt.Errorf("failed to backfill: target channel %d not in outcome", opts.TargetChannelID), "missing target channel", backfillCID}
	}
	res, err := targetChannelTimeResolution(target)
	if err != nil {
		return 0, 0, HistoryBackfillOpts{}, &UnreportableChannelError{fmt.Errorf("failed to backfill: %w", err), "invalid target channel time resolution opts", backfillCID}
	}
	watermark, ok := out.ValidAfterNanoseconds[backfillCID]
	if !ok {
		return 0, 0, HistoryBackfillOpts{}, &UnreportableChannelError{nil, "IsReportable=false; no ValidAfterNanoseconds entry yet, this must be a new channel", backfillCID}
	}

	obsTSNanos := out.ObservationTimestampNanoseconds

	var bestRaw uint64
	var bestNanos uint64
	found := false
	for rawKey := range opts.Observations {
		tsN := ObservationTimestampKeyToNanoseconds(rawKey, res)
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
		return 0, 0, HistoryBackfillOpts{}, &UnreportableChannelError{nil, "backfill complete, no remaining timestamps", backfillCID}
	}

	return bestNanos, bestRaw, opts, nil
}
