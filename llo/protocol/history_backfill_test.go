package protocol

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_ObservationTimestampKeyToNanoseconds_Overflow(t *testing.T) {
	// Scaling wraps, and it wraps in the dangerous direction: callers compare
	// the result against an upper bound it must be below, so a wrapped
	// far-future key would read as a valid past timestamp.
	for _, tc := range []struct {
		name   string
		rawKey uint64
		res    TimeResolution
		want   uint64
		wantOK bool
	}{
		{"seconds", 1_700_000_000, ResolutionSeconds, uint64(1_700_000_000) * uint64(1e9), true},
		{"largest representable second", math.MaxUint64 / uint64(1e9), ResolutionSeconds, (math.MaxUint64 / uint64(1e9)) * 1e9, true},
		{"seconds overflow", math.MaxUint64/uint64(1e9) + 1, ResolutionSeconds, 0, false},
		{"milliseconds overflow", math.MaxUint64/uint64(1e6) + 1, ResolutionMilliseconds, 0, false},
		{"microseconds overflow", math.MaxUint64/uint64(1e3) + 1, ResolutionMicroseconds, 0, false},
		{"nanoseconds never overflow", math.MaxUint64, ResolutionNanoseconds, math.MaxUint64, true},
		{"unknown resolution is treated as seconds", math.MaxUint64, TimeResolution(200), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ObservationTimestampKeyToNanoseconds(tc.rawKey, tc.res)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, got)
		})
	}

	// The wrapped value is what makes this worth guarding: it is far below any
	// plausible reference time, so every "must be in the past" check it is fed
	// to would pass. Computed through variables because the constant expression
	// does not compile.
	rawKey, multiplier := math.MaxUint64/uint64(1e9)+1, uint64(1e9)
	require.Less(t, rawKey*multiplier, uint64(1_700_000_000)*multiplier)
}

func Test_GetHistoryBackfillOpts(t *testing.T) {
	raw := llotypes.ChannelOpts(`{"targetChannelId":7,"observations":{"1700000000":{"1":"1.5"}}}`)
	cd := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatHistoryBackfill, Opts: raw}

	want, err := ParseHistoryBackfillOpts(raw)
	require.NoError(t, err)

	t.Run("no cache parses the definition", func(t *testing.T) {
		got, err := GetHistoryBackfillOpts(nil, cd, 1)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
	t.Run("cache returns the same opts", func(t *testing.T) {
		c := NewOptsCache()
		c.Set(1, raw)
		got, err := GetHistoryBackfillOpts(c, cd, 1)
		require.NoError(t, err)
		require.Equal(t, want, got)

		// Served from the decoded entry the second time; same value.
		got, err = GetHistoryBackfillOpts(c, cd, 1)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
	t.Run("a channel missing from the cache falls back to the definition", func(t *testing.T) {
		got, err := GetHistoryBackfillOpts(NewOptsCache(), cd, 1)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
	t.Run("empty opts are rejected even though they decode", func(t *testing.T) {
		// GetOpts skips the unmarshal for empty raw bytes and yields the zero
		// value without error, which the post-decode rules have to catch.
		c := NewOptsCache()
		c.Set(2, nil)
		_, err := GetHistoryBackfillOpts(c, llotypes.ChannelDefinition{}, 2)
		require.Error(t, err)
	})
}

func Test_ValidateHistoryBackfillTarget(t *testing.T) {
	backfill := func(targetID llotypes.ChannelID) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			Opts:         llotypes.ChannelOpts(fmt.Sprintf(`{"targetChannelId":%d,"observations":{"1700000000":{"1":"1.5"}}}`, targetID)),
		}
	}
	reportable := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
		Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
	}

	t.Run("a reportable target is accepted", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: backfill(2), 2: reportable}
		require.NoError(t, ValidateHistoryBackfillTarget(defs[1], defs))
	})
	t.Run("targeting itself is rejected", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: backfill(1)}
		require.ErrorContains(t, ValidateHistoryBackfillTarget(defs[1], defs),
			"is itself a history_backfill channel")
	})
	t.Run("targeting another backfill channel is rejected", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: backfill(2), 2: backfill(3), 3: reportable}
		require.ErrorContains(t, ValidateHistoryBackfillTarget(defs[1], defs),
			"is itself a history_backfill channel")
	})

	// The rule is admission-only: a committed self-targeting channel must not
	// stop an oracle from observing, but it must not be installable either.
	t.Run("committed definitions keep working", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: backfill(1)}
		codecs := map[llotypes.ReportFormat]ReportCodec{}
		require.NoError(t, VerifyChannelDefinitions(codecs, defs))
		err := VerifyChannelDefinitionsForAdmission(codecs, defs, map[llotypes.ChannelID]struct{}{1: {}})
		require.ErrorContains(t, err, "is itself a history_backfill channel")
	})
}

func Test_ValidateHistoryBackfillValues(t *testing.T) {
	backfill := func(agg llotypes.Aggregator, value string) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: agg}},
			Opts:         llotypes.ChannelOpts(fmt.Sprintf(`{"targetChannelId":2,"observations":{"1700000000":{"1":%q}}}`, value)),
		}
	}
	target := func(agg llotypes.Aggregator) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: agg}},
		}
	}
	// 59 nines is 196 bits, one digit past what MaxDecimalCoefficientBits admits.
	oversized := strings.Repeat("9", 59)

	for _, tc := range []struct {
		name    string
		agg     llotypes.Aggregator
		value   string
		wantErr string
	}{
		{"a decimal is accepted", llotypes.AggregatorMedian, "1.5", ""},
		{"a valid quote is accepted", llotypes.AggregatorQuote, "Q{Bid: 1, Benchmark: 2, Ask: 3}", ""},
		{"an unparseable decimal is rejected", llotypes.AggregatorMedian, "not a number", "timestamp 1700000000: stream 1:"},
		{"an oversized decimal coefficient is rejected", llotypes.AggregatorMedian, oversized, ErrDecimalCoefficientOutOfRange.Error()},
		{"an oversized quote coefficient is rejected", llotypes.AggregatorQuote, fmt.Sprintf("Q{Bid: 1, Benchmark: 2, Ask: %s}", oversized), ErrDecimalCoefficientOutOfRange.Error()},
		{"an inverted quote is rejected", llotypes.AggregatorQuote, "Q{Bid: 3, Benchmark: 2, Ask: 1}", "quote violates bid <= benchmark <= ask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defs := llotypes.ChannelDefinitions{1: backfill(tc.agg, tc.value), 2: target(tc.agg)}
			err := ValidateHistoryBackfillValues(defs[1], defs)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	t.Run("a missing target is left to ValidateHistoryBackfillAgainstDefinitions", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: backfill(llotypes.AggregatorMedian, "not a number")}
		require.NoError(t, ValidateHistoryBackfillValues(defs[1], defs))
	})

	// The rule is admission-only: committed rows are left to the runtime path,
	// but no new one is installable.
	t.Run("committed definitions keep working", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: backfill(llotypes.AggregatorQuote, "Q{Bid: 3, Benchmark: 2, Ask: 1}"), 2: target(llotypes.AggregatorQuote)}
		codecs := map[llotypes.ReportFormat]ReportCodec{}
		require.NoError(t, VerifyChannelDefinitions(codecs, defs))
		err := VerifyChannelDefinitionsForAdmission(codecs, defs, map[llotypes.ChannelID]struct{}{1: {}})
		require.ErrorContains(t, err, "quote violates bid <= benchmark <= ask")
	})
}
