package llo

import (
	. "github.com/smartcontractkit/chainlink-data-streams/llo"

	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func TestSelectBackfillCandidate_picksSmallestEligible(t *testing.T) {
	streams := []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}
	targetID := llotypes.ChannelID(2)
	backfillID := llotypes.ChannelID(10)
	optsJSON := []byte(`{"targetChannelId":2,"observations":{"100":{"1":"1"},"200":{"1":"2"},"150":{"1":"3"}}}`)

	out := Outcome{
		ObservationTimestampNanoseconds: uint64(300 * time.Second),
		ChannelDefinitions: llotypes.ChannelDefinitions{
			targetID: {ReportFormat: llotypes.ReportFormatJSON, Streams: streams},
			backfillID: {
				ReportFormat: llotypes.ReportFormatHistoryBackfill,
				Streams:      streams,
				Opts:         optsJSON,
			},
		},
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{backfillID: 0},
	}

	ts, raw, _, uerr := SelectBackfillCandidate(&out, backfillID)
	require.Nil(t, uerr)
	require.Equal(t, uint64(100*time.Second), ts)
	require.Equal(t, uint64(100), raw)

	out.ValidAfterNanoseconds[backfillID] = uint64(100 * time.Second)
	ts2, raw2, _, uerr2 := SelectBackfillCandidate(&out, backfillID)
	require.Nil(t, uerr2)
	require.Equal(t, uint64(150*time.Second), ts2)
	require.Equal(t, uint64(150), raw2)

	out.ValidAfterNanoseconds[backfillID] = uint64(200 * time.Second)
	_, _, _, uerr3 := SelectBackfillCandidate(&out, backfillID)
	require.NotNil(t, uerr3)
	require.Contains(t, uerr3.Error(), "backfill complete, no remaining timestamps")
}

func TestSelectBackfillCandidate_skipsFutureObservations(t *testing.T) {
	streams := []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}
	optsJSON := []byte(`{"targetChannelId":2,"observations":{"500":{"1":"1"}}}`)
	out := Outcome{
		ObservationTimestampNanoseconds: uint64(400 * time.Second),
		ChannelDefinitions: llotypes.ChannelDefinitions{
			2: {ReportFormat: llotypes.ReportFormatJSON, Streams: streams},
			10: {
				ReportFormat: llotypes.ReportFormatHistoryBackfill,
				Streams:      streams,
				Opts:         optsJSON,
			},
		},
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{10: 0},
	}
	_, _, _, uerr := SelectBackfillCandidate(&out, 10)
	require.NotNil(t, uerr)
	require.Contains(t, uerr.Error(), "backfill complete, no remaining timestamps")
}

func TestValidateHistoryBackfillAgainstDefinitions_invalidTargetOpts(t *testing.T) {
	streams := []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}
	defs := llotypes.ChannelDefinitions{
		2: {
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked,
			Streams:      streams,
			Opts:         []byte(`not-json`),
		},
		10: {
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Streams:      streams,
			Opts:         []byte(`{"targetChannelId":2,"observations":{"1000":{"1":"1"}}}`),
		},
	}
	err := ValidateHistoryBackfillAgainstDefinitions(defs[10], defs, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "target channel opts")
}

func TestDropInvalidHistoryBackfillChannels_dropsInvalid(t *testing.T) {
	tests.Context(t)
	lggr := logger.Test(t)
	streams := []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}
	defs := llotypes.ChannelDefinitions{
		2: {ReportFormat: llotypes.ReportFormatJSON, Streams: streams},
		10: {
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Streams:      streams,
			Opts:         []byte(`{"targetChannelId":99,"observations":{"1":{"1":"1"}}}`),
		},
	}
	ref := uint64(time.Now().Add(time.Hour).UnixNano())
	out := DropInvalidHistoryBackfillChannels(lggr, defs, ref)
	_, ok := out[10]
	require.False(t, ok)
	_, still := defs[10]
	require.True(t, still, "input definitions map must not be mutated")
}

func FuzzParseHistoryBackfillOpts(f *testing.F) {
	f.Add([]byte(`{"targetChannelId":1,"observations":{"1":{"1":"1"}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		t.Parallel()
		_, _ = ParseHistoryBackfillOpts(data)
	})
}
