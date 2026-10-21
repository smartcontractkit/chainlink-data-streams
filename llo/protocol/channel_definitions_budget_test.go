package protocol

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_ChannelDefinitionsBudget(t *testing.T) {
	channel := func(optsBytes int, streamIDs ...llotypes.StreamID) llotypes.ChannelDefinition {
		cd := llotypes.ChannelDefinition{Opts: []byte(strings.Repeat("x", optsBytes))}
		for _, id := range streamIDs {
			cd.Streams = append(cd.Streams, llotypes.Stream{StreamID: id, Aggregator: llotypes.AggregatorMedian})
		}
		return cd
	}
	// Every channel at MaxChannelOptsBytes, so the set sits exactly at
	// MaxTotalOptsBytes with no other budget involved.
	atOptsLimit := func() llotypes.ChannelDefinitions {
		defs := llotypes.ChannelDefinitions{}
		for c := range MaxTotalOptsBytes / MaxChannelOptsBytes {
			defs[llotypes.ChannelID(c+1)] = channel(MaxChannelOptsBytes, 1)
		}
		return defs
	}

	t.Run("refuses a change that grows the set past a budget", func(t *testing.T) {
		defs := atOptsLimit()
		b := NewChannelDefinitionsBudget(nil, defs, nil)
		require.NoError(t, b.Err())

		require.False(t, b.Replace(1000, nil, channel(1, 1)))
		prev := defs[1]
		require.False(t, b.Replace(1, &prev, channel(MaxChannelOptsBytes+1, 1)))
		// A refused change leaves the totals as they were.
		require.NoError(t, b.Err())

		// A change that stays within the budget is accepted.
		require.True(t, b.Replace(1000, nil, channel(0, 1)))
		require.True(t, b.Replace(1, &prev, channel(MaxChannelOptsBytes, 2)))
		require.NoError(t, b.Err())
	})

	t.Run("lets a set already over budget stay or shrink", func(t *testing.T) {
		defs := atOptsLimit()
		defs[1000] = channel(MaxChannelOptsBytes, 1)
		b := NewChannelDefinitionsBudget(nil, defs, nil)
		require.ErrorContains(t, b.Err(), "too many opts bytes across all channels")

		prev := defs[1000]
		require.False(t, b.Replace(1000, &prev, channel(MaxChannelOptsBytes+1, 1)))
		replaced := channel(MaxChannelOptsBytes, 2)
		require.True(t, b.Replace(1000, &prev, replaced))

		prev = replaced
		trimmed := channel(0, 2)
		trimmed.Tombstone = true
		require.True(t, b.Replace(1000, &prev, trimmed))
		require.NoError(t, b.Err())
	})

	t.Run("a pair is released only when no channel names it", func(t *testing.T) {
		shared := channel(0, 1)
		b := NewChannelDefinitionsBudget(nil, llotypes.ChannelDefinitions{1: shared, 2: shared}, nil)
		require.Len(t, b.aggregatedPairs, 1)

		require.True(t, b.Replace(1, &shared, channel(0, 2)))
		require.Len(t, b.aggregatedPairs, 2)
		require.True(t, b.Replace(2, &shared, channel(0, 2)))
		require.Len(t, b.aggregatedPairs, 1)
	})

	t.Run("tombstones and backfill channels hold no aggregate", func(t *testing.T) {
		tombstone := channel(0, 1)
		tombstone.Tombstone = true
		backfill := channel(0, 2)
		backfill.ReportFormat = llotypes.ReportFormatHistoryBackfill
		calculated := llotypes.ChannelDefinition{Streams: []llotypes.Stream{{StreamID: 3, Aggregator: llotypes.AggregatorCalculated}}}

		b := NewChannelDefinitionsBudget(nil, llotypes.ChannelDefinitions{1: tombstone, 2: backfill, 3: calculated}, nil)
		require.Empty(t, b.aggregatedPairs)
		// Their stream entries are persisted all the same.
		require.Equal(t, 3, b.streamEntries)
	})

	// expression declares one calculated stream per ID in its opts.
	expression := func(observed llotypes.StreamID, calculatedIDs ...llotypes.StreamID) llotypes.ChannelDefinition {
		abi := make([]string, 0, len(calculatedIDs))
		for _, id := range calculatedIDs {
			abi = append(abi, fmt.Sprintf(`{"expressionStreamID":%d}`, id))
		}
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams:      []llotypes.Stream{{StreamID: observed, Aggregator: llotypes.AggregatorMedian}},
			Opts:         llotypes.ChannelOpts(fmt.Sprintf(`{"abi":[%s]}`, strings.Join(abi, ","))),
		}
	}

	t.Run("refuses calculated streams past the budget", func(t *testing.T) {
		ids := make([]llotypes.StreamID, MaxTotalCalculatedStreams)
		for i := range ids {
			ids[i] = llotypes.StreamID(1_000_000 + i)
		}
		b := NewChannelDefinitionsBudget(nil, llotypes.ChannelDefinitions{1: expression(1, ids...)}, nil)
		require.NoError(t, b.Err())

		require.False(t, b.Replace(2, nil, expression(2, 2_000_000)))
		require.True(t, b.Replace(2, nil, expression(2)))
		require.NoError(t, b.Err())
	})

	t.Run("refuses TWAP calls past the budget", func(t *testing.T) {
		codecs := map[llotypes.ReportFormat]ReportCodec{0: twapCodec{}}
		defs := llotypes.ChannelDefinitions{}
		for c := range MaxTotalTWAPCalls / MaxTWAPCallsPerExpression {
			defs[llotypes.ChannelID(c+1)] = channel(0, 1)
		}
		b := NewChannelDefinitionsBudget(codecs, defs, nil)
		require.NoError(t, b.Err())

		require.False(t, b.Replace(1000, nil, channel(0, 1)))
		tombstone := channel(0, 1)
		tombstone.Tombstone = true
		require.True(t, b.Replace(1000, nil, tombstone))
		require.NoError(t, b.Err())
	})

	t.Run("refuses unique stream IDs past the budget", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{}
		for c := range MaxObservationStreamValuesLength {
			defs[llotypes.ChannelID(c+1)] = channel(0, llotypes.StreamID(c+1))
		}
		b := NewChannelDefinitionsBudget(nil, defs, nil)

		require.False(t, b.Replace(100_000, nil, channel(0, 100_000)))
		// A stream already observed adds no ID.
		require.True(t, b.Replace(100_000, nil, channel(0, 1)))
	})

	t.Run("refuses a calculated stream ID that another owner claims", func(t *testing.T) {
		b := NewChannelDefinitionsBudget(nil, llotypes.ChannelDefinitions{1: expression(1, 100)}, nil)

		require.False(t, b.Replace(2, nil, expression(2, 100)))
		require.False(t, b.Replace(2, nil, channel(0, 100)))
		require.True(t, b.Replace(2, nil, expression(2, 101)))

		// Moving the declaration within one change is not a clash.
		prev := expression(1, 100)
		require.True(t, b.Replace(1, &prev, expression(1, 102)))
		require.True(t, b.Replace(3, nil, channel(0, 100)))
	})

	t.Run("a set that already clashes may keep its clash", func(t *testing.T) {
		b := NewChannelDefinitionsBudget(nil, llotypes.ChannelDefinitions{1: expression(1, 100), 2: channel(0, 100)}, nil)
		require.Equal(t, 1, b.clashes)

		prev := channel(0, 100)
		require.True(t, b.Replace(2, &prev, channel(1, 100)))
		require.False(t, b.Replace(3, nil, expression(3, 100)))
		require.Equal(t, 1, b.clashes)
	})
}
