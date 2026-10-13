package protocol

import (
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
		b := NewChannelDefinitionsBudget(defs)
		require.NoError(t, b.Err())

		require.False(t, b.Replace(nil, channel(1, 1)))
		prev := defs[1]
		require.False(t, b.Replace(&prev, channel(MaxChannelOptsBytes+1, 1)))
		// A refused change leaves the totals as they were.
		require.NoError(t, b.Err())

		// A change that stays within the budget is accepted.
		require.True(t, b.Replace(nil, channel(0, 1)))
		require.True(t, b.Replace(&prev, channel(MaxChannelOptsBytes, 2)))
		require.NoError(t, b.Err())
	})

	t.Run("lets a set already over budget stay or shrink", func(t *testing.T) {
		defs := atOptsLimit()
		defs[1000] = channel(MaxChannelOptsBytes, 1)
		b := NewChannelDefinitionsBudget(defs)
		require.ErrorContains(t, b.Err(), "too many opts bytes across all channels")

		prev := defs[1000]
		require.False(t, b.Replace(&prev, channel(MaxChannelOptsBytes+1, 1)))
		replaced := channel(MaxChannelOptsBytes, 2)
		require.True(t, b.Replace(&prev, replaced))

		prev = replaced
		trimmed := channel(0, 2)
		trimmed.Tombstone = true
		require.True(t, b.Replace(&prev, trimmed))
		require.NoError(t, b.Err())
	})

	t.Run("a pair is released only when no channel names it", func(t *testing.T) {
		shared := channel(0, 1)
		b := NewChannelDefinitionsBudget(llotypes.ChannelDefinitions{1: shared, 2: shared})
		require.Len(t, b.aggregatedPairs, 1)

		require.True(t, b.Replace(&shared, channel(0, 2)))
		require.Len(t, b.aggregatedPairs, 2)
		require.True(t, b.Replace(&shared, channel(0, 2)))
		require.Len(t, b.aggregatedPairs, 1)
	})

	t.Run("tombstones and backfill channels hold no aggregate", func(t *testing.T) {
		tombstone := channel(0, 1)
		tombstone.Tombstone = true
		backfill := channel(0, 2)
		backfill.ReportFormat = llotypes.ReportFormatHistoryBackfill
		calculated := llotypes.ChannelDefinition{Streams: []llotypes.Stream{{StreamID: 3, Aggregator: llotypes.AggregatorCalculated}}}

		b := NewChannelDefinitionsBudget(llotypes.ChannelDefinitions{1: tombstone, 2: backfill, 3: calculated})
		require.Empty(t, b.aggregatedPairs)
		// Their stream entries are persisted all the same.
		require.Equal(t, 3, b.streamEntries)
	})
}
