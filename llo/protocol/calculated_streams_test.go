package protocol

import (
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

const twoExpressionOpts = `{"abi":[
	{"type":"int256","expression":"Add(s1, s2)","expressionStreamID":998},
	{"type":"int256","expression":"Sub(s1, s2)","expressionStreamID":999}
]}`

func observedStreams() []llotypes.Stream {
	return []llotypes.Stream{
		{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
		{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
	}
}

func exprChannel(opts string, streams []llotypes.Stream) llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Opts:         []byte(opts),
		Streams:      streams,
	}
}

func TestEffectiveStreams(t *testing.T) {
	t.Run("non-expression channel is returned unchanged", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      observedStreams(),
		}
		got, err := EffectiveStreams(nil, cd, 1)
		require.NoError(t, err)
		require.Equal(t, cd.Streams, got)
	})

	t.Run("appends declared calculated streams in declaration order", func(t *testing.T) {
		got, err := EffectiveStreams(nil, exprChannel(twoExpressionOpts, observedStreams()), 1)
		require.NoError(t, err)
		require.Equal(t, []llotypes.Stream{
			{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 998, Aggregator: llotypes.AggregatorCalculated},
			{StreamID: 999, Aggregator: llotypes.AggregatorCalculated},
		}, got)
	})

	t.Run("a definition carrying the streams inline derives identically", func(t *testing.T) {
		// The shape written by the v3.0 append, which v3.1 must tolerate reading.
		inline := append(observedStreams(),
			llotypes.Stream{StreamID: 998, Aggregator: llotypes.AggregatorCalculated},
			llotypes.Stream{StreamID: 999, Aggregator: llotypes.AggregatorCalculated},
		)
		fromInline, err := EffectiveStreams(nil, exprChannel(twoExpressionOpts, inline), 1)
		require.NoError(t, err)
		fromClean, err := EffectiveStreams(nil, exprChannel(twoExpressionOpts, observedStreams()), 1)
		require.NoError(t, err)
		require.Equal(t, fromClean, fromInline)
	})

	t.Run("is idempotent under repeated application", func(t *testing.T) {
		cd := exprChannel(twoExpressionOpts, observedStreams())
		once, err := EffectiveStreams(nil, cd, 1)
		require.NoError(t, err)
		cd.Streams = once
		twice, err := EffectiveStreams(nil, cd, 1)
		require.NoError(t, err)
		require.Equal(t, once, twice)
	})

	t.Run("stale inline streams do not survive an opts change", func(t *testing.T) {
		// A channel whose expressions were re-voted: the definition still lists
		// the old calculated stream, but only the newly declared one is derived.
		stale := append(observedStreams(),
			llotypes.Stream{StreamID: 111, Aggregator: llotypes.AggregatorCalculated},
		)
		cd := exprChannel(`{"abi":[{"type":"int256","expression":"Add(s1, s2)","expressionStreamID":222}]}`, stale)
		got, err := EffectiveStreams(nil, cd, 1)
		require.NoError(t, err)
		require.Equal(t, []llotypes.Stream{
			{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 222, Aggregator: llotypes.AggregatorCalculated},
		}, got)
	})

	t.Run("the opts cache and the raw opts agree", func(t *testing.T) {
		cd := exprChannel(twoExpressionOpts, observedStreams())
		cache := NewOptsCache()
		cache.ResetTo(llotypes.ChannelDefinitions{1: cd})

		cached, err := EffectiveStreams(cache, cd, 1)
		require.NoError(t, err)
		raw, err := EffectiveStreams(nil, cd, 1)
		require.NoError(t, err)
		require.Equal(t, raw, cached)
	})

	t.Run("falls back to the raw opts on a cache miss", func(t *testing.T) {
		cd := exprChannel(twoExpressionOpts, observedStreams())
		got, err := EffectiveStreams(NewOptsCache(), cd, 1)
		require.NoError(t, err)
		require.Len(t, got, 4)
	})

	t.Run("rejects undecodable opts", func(t *testing.T) {
		_, err := EffectiveStreams(nil, exprChannel(`not json`, observedStreams()), 7)
		require.ErrorContains(t, err, "channel 7")
		require.ErrorContains(t, err, "failed to decode calculated stream opts")
	})

	t.Run("rejects opts declaring no expressions", func(t *testing.T) {
		_, err := EffectiveStreams(nil, exprChannel(`{"abi":[]}`, observedStreams()), 7)
		require.ErrorContains(t, err, "no expressions found in channel definition")
	})

	t.Run("rejects a zero expression stream ID", func(t *testing.T) {
		cd := exprChannel(`{"abi":[{"type":"int256","expression":"s1","expressionStreamID":0}]}`, observedStreams())
		_, err := EffectiveStreams(nil, cd, 7)
		require.ErrorContains(t, err, "expression stream ID is 0")
	})
}

func TestCalculatedStreamIDs(t *testing.T) {
	t.Run("returns the declared IDs in order", func(t *testing.T) {
		ids, err := CalculatedStreamIDs(nil, exprChannel(twoExpressionOpts, observedStreams()), 1)
		require.NoError(t, err)
		require.Equal(t, []llotypes.StreamID{998, 999}, ids)
	})

	t.Run("returns the IDs decoded before a zero ID alongside the error", func(t *testing.T) {
		cd := exprChannel(`{"abi":[
			{"type":"int256","expression":"s1","expressionStreamID":998},
			{"type":"int256","expression":"s2","expressionStreamID":0}
		]}`, observedStreams())
		ids, err := CalculatedStreamIDs(nil, cd, 1)
		require.ErrorContains(t, err, "expression stream ID is 0")
		require.Equal(t, []llotypes.StreamID{998}, ids)
	})

	t.Run("agrees with EffectiveStreams", func(t *testing.T) {
		cd := exprChannel(twoExpressionOpts, observedStreams())
		ids, err := CalculatedStreamIDs(nil, cd, 1)
		require.NoError(t, err)
		streams, err := EffectiveStreams(nil, cd, 1)
		require.NoError(t, err)

		derived := make([]llotypes.StreamID, 0, len(ids))
		for _, strm := range streams {
			if strm.Aggregator == llotypes.AggregatorCalculated {
				derived = append(derived, strm.StreamID)
			}
		}
		require.Equal(t, ids, derived)
	})
}

func TestHasCalculatedStreams(t *testing.T) {
	require.True(t, HasCalculatedStreams(llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr}))
	require.False(t, HasCalculatedStreams(llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatEVMPremiumLegacy}))
	// The stream list does not answer the question; the report format does.
	require.False(t, HasCalculatedStreams(llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
		Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorCalculated}},
	}))
}
