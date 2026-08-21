package protocol

import (
	"errors"
	"fmt"
	"math"
	"sort"
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
	if len(w.Observations) > MaxHistoryBackfillObservations {
		return fmt.Errorf("backfill definition has too many observations: %d > %d",
			len(w.Observations), MaxHistoryBackfillObservations)
	}

	// Keys are visited in sorted order so that a definition with more than one
	// problem always reports the same one, and a duplicate always names the same
	// pair. Distinct keys can decode to the same number -- ParseUint accepts
	// leading zeros, so "01" and "1" are both 1 -- which is what the duplicate
	// checks below are for.
	o.Observations = make(map[uint64]map[llotypes.StreamID]string, len(w.Observations))
	firstTSKey := make(map[uint64]string, len(w.Observations))
	for _, tsStr := range sortedKeys(w.Observations) {
		streams := w.Observations[tsStr]
		ts, err := strconv.ParseUint(tsStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid observation timestamp key %q: %w", tsStr, err)
		}

		if earlier, ok := firstTSKey[ts]; ok {
			return fmt.Errorf("duplicate timestamp key %q: decodes to %d, same as %q", tsStr, ts, earlier)
		}
		firstTSKey[ts] = tsStr

		if len(streams) == 0 {
			return fmt.Errorf("empty stream map for timestamp %s", tsStr)
		}

		if len(streams) > MaxStreamsPerChannel {
			return fmt.Errorf("backfill observation has too many streams: %d > %d", len(streams), MaxStreamsPerChannel)
		}

		inner := make(map[llotypes.StreamID]string, len(streams))
		firstStreamKey := make(map[llotypes.StreamID]string, len(streams))
		for _, sidStr := range sortedKeys(streams) {
			sid64, err := strconv.ParseUint(sidStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid stream id key %q at timestamp %s: %w", sidStr, tsStr, err)
			}

			sid := llotypes.StreamID(sid64)
			if earlier, ok := firstStreamKey[sid]; ok {
				return fmt.Errorf("duplicate stream id key %q at timestamp %s: decodes to %d, same as %q", sidStr, tsStr, sid, earlier)
			}
			firstStreamKey[sid] = sidStr
			inner[sid] = streams[sidStr]
		}
		o.Observations[ts] = inner
	}
	return nil
}

// sortedKeys returns a map's keys in ascending order, so that a loop over them
// reports the same problem on every oracle.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	if err := o.validate(); err != nil {
		return o, err
	}
	return o, nil
}

// GetHistoryBackfillOpts returns a history_backfill channel's parsed opts,
// preferring the (node-local) decode cache and falling back to parsing the
// definition's raw opts on a miss. The fallback keeps the result identical
// across oracles even when the cache has not been populated.
//
// Selection runs two or three times per backfill channel per round, and the opts
// carry up to MaxHistoryBackfillObservations observations of stream values, so
// parsing them afresh each time is the difference between one allocation of that
// map per generation and several per round.
func GetHistoryBackfillOpts(optsCache *OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) (HistoryBackfillOpts, error) {
	if optsCache == nil {
		return ParseHistoryBackfillOpts(cd.Opts)
	}
	o, err := GetOpts[HistoryBackfillOpts](optsCache, cid)
	if err != nil {
		return ParseHistoryBackfillOpts(cd.Opts)
	}
	// GetOpts decodes but does not apply the rules ParseHistoryBackfillOpts
	// applies after decoding, including the ones that reject empty opts: a
	// channel with no raw bytes decodes to the zero value without error.
	if err := o.validate(); err != nil {
		return HistoryBackfillOpts{}, err
	}
	return o, nil
}

// validate applies the rules that hold for decoded opts however they were
// decoded.
func (o HistoryBackfillOpts) validate() error {
	if o.TargetChannelID == 0 {
		return errors.New("targetChannelId must be non-zero")
	}
	if len(o.Observations) == 0 {
		return errors.New("observations must be non-empty")
	}
	return nil
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
		res, err := TargetChannelTimeResolution(target)
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

// ObservationTimestampKeyToNanoseconds converts a raw observation timestamp key
// from opts to nanoseconds, reporting whether the key is representable.
//
// The scaling is unsigned multiplication, so it wraps: a key beyond about 1.8e10
// seconds becomes a small number of nanoseconds. That is the dangerous direction
// -- every caller compares the result against a bound it must be below (now, the
// round's observation timestamp) and a wrapped value passes all of them, so an
// unrepresentable far-future timestamp would read as a valid past one. Keys come
// from channel definition opts, which makes this reachable from configuration.
func ObservationTimestampKeyToNanoseconds(rawKey uint64, res TimeResolution) (nanoseconds uint64, ok bool) {
	var multiplier uint64
	switch res {
	case ResolutionMilliseconds:
		multiplier = 1e6
	case ResolutionMicroseconds:
		multiplier = 1e3
	case ResolutionNanoseconds:
		return rawKey, true
	case ResolutionSeconds:
		multiplier = 1e9
	default:
		multiplier = 1e9
	}
	if rawKey > math.MaxUint64/multiplier {
		return 0, false
	}
	return rawKey * multiplier, true
}

func TargetChannelTimeResolution(target llotypes.ChannelDefinition) (TimeResolution, error) {
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
	// The target's report format is what says whether it has calculated
	// streams: they are derived from its opts, not listed on the definition.
	if HasCalculatedStreams(target) {
		return errors.New("history backfill target channel must not use calculated streams (phase 1 limitation)")
	}
	res, err := TargetChannelTimeResolution(target)
	if err != nil {
		return err
	}
	for rawTS, streams := range opts.Observations {
		tsNanos, ok := ObservationTimestampKeyToNanoseconds(rawTS, res)
		if !ok {
			return fmt.Errorf("observation timestamp raw %d cannot be expressed in nanoseconds at the target channel's time resolution", rawTS)
		}
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

// ValidateHistoryBackfillTarget rejects a backfill channel whose target is
// itself a backfill channel, which includes a channel targeting itself.
//
// Such a definition passes every other rule: the resolution lookup falls through
// to seconds, the stream lists match trivially when the target is the channel
// itself, and the target declares no calculated streams. It fails only at the
// very end, in Report, where the codec resolved from the target's report format
// is ReportCodecHistoryBackfill, whose Encode always errors. The channel is then
// selected, logged and skipped every round forever, because the watermark only
// advances on a report that was actually emitted.
//
// This is an admission-only rule (see VerifyChannelDefinitionsForAdmission).
// Rejecting an already-committed definition here would stop an oracle from
// observing at all, and a definition like this has never produced a report, so
// there is nothing to protect but the ability to install a new one.
func ValidateHistoryBackfillTarget(cd llotypes.ChannelDefinition, defs llotypes.ChannelDefinitions) error {
	opts, err := ParseHistoryBackfillOpts(cd.Opts)
	if err != nil {
		return err
	}
	target, ok := defs[opts.TargetChannelID]
	if !ok {
		return nil // reported by ValidateHistoryBackfillAgainstDefinitions
	}
	if target.ReportFormat == llotypes.ReportFormatHistoryBackfill {
		return fmt.Errorf("target channel %d is itself a history_backfill channel, whose reports cannot be encoded", opts.TargetChannelID)
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
