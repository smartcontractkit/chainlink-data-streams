package llo

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm"
)

// newDeterminismTestPlugin returns a plugin in the state a freshly started
// process is in: an empty OptsCache.
func newDeterminismTestPlugin(t *testing.T) *Plugin {
	return &Plugin{
		Config:       Config{VerboseLogging: true},
		OutcomeCodec: protoOutcomeCodecV1{},
		Logger:       logger.Test(t),
		ReportCodecs: map[llotypes.ReportFormat]protocol.ReportCodec{
			llotypes.ReportFormatEVMABIEncodeUnpacked: evm.NewReportCodecEVMABIEncodeUnpacked(logger.Nop(), 1),
		},
		RetirementReportCodec:               protocol.StandardRetirementReportCodec{},
		DefaultMinReportIntervalNanoseconds: 0,
		ProtocolVersion:                     1,
		OptsCache:                           protocol.NewOptsCache(),
	}
}

// Test_Reports_DeterministicAcrossOptsCacheState asserts that Reports() is a
// pure function of (seqNr, outcome): two oracles given the same committed
// outcome must return the same reports, whether or not they previously computed
// that outcome themselves.
//
// This is reachable in production. libocr calls Reports() on committed outcomes
// delivered to the report attestation loop, and a restarted oracle catching up
// accepts a CertifiedCommit straight out of an epoch-start proof
// (outcome_generation_follower.go) without ever running Outcome() for that
// sequence number. Its p.OptsCache is therefore still empty.
//
// Before the fix the report codec read its options exclusively from
// p.OptsCache, so on a cold cache Encode failed, Reports() logged and skipped
// the channel, and the two oracles disagreed.
func Test_Reports_DeterministicAcrossOptsCacheState(t *testing.T) {
	ctx := tests.Context(t)

	const channelID = llotypes.ChannelID(1)
	const seqNr = uint64(2)

	// Stream 0 is the native token price, stream 1 the link token price, and
	// the remaining streams map one-to-one onto the ABI elements.
	opts := []byte(`{` +
		`"baseUSDFee":"0.1",` +
		`"expirationWindow":3600,` +
		`"feedID":"0x0003111111111111111111111111111111111111111111111111111111111111",` +
		`"abi":[{"type":"int192","multiplier":"1"}],` +
		`"TimeResolution":"s"` +
		`}`)

	cd := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked,
		Streams: []llotypes.Stream{
			{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 3, Aggregator: llotypes.AggregatorMedian},
		},
		Opts: opts,
	}

	outcome := Outcome{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: uint64(2000 * time.Second),
		ChannelDefinitions:              llotypes.ChannelDefinitions{channelID: cd},
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{channelID: uint64(1999 * time.Second)},
		StreamAggregates: protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
			2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
			3: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(7))},
		},
	}

	warm := newDeterminismTestPlugin(t)
	// An oracle that computed this outcome itself. This is the state Outcome()
	// leaves behind (see plugin_outcome.go, "Reset OptsCache").
	warm.OptsCache.ResetTo(outcome.ChannelDefinitions)

	// An oracle that restarted and is serving Reports() for a committed outcome
	// it received via epoch-start catch-up, so it never ran Outcome().
	cold := newDeterminismTestPlugin(t)

	encoded, err := warm.OutcomeCodec.Encode(outcome)
	require.NoError(t, err)

	warmReports, err := warm.Reports(ctx, seqNr, encoded)
	require.NoError(t, err)
	coldReports, err := cold.Reports(ctx, seqNr, encoded)
	require.NoError(t, err)

	require.Len(t, warmReports, 1, "sanity check: the warm oracle should emit one report")
	assert.Equal(t, warmReports, coldReports,
		"Reports() must be a function of the committed outcome alone, but the cold oracle produced a different report set")
}

// Test_Reports_SecondsResolutionDeterministicAcrossOptsCacheState covers the
// second way a cold cache changed Reports() output: IsSecondsResolution read
// TimeResolution from the cache and returned false on a miss, so a cold oracle
// skipped the seconds-overlap check that a warm oracle applied and reported a
// channel its peers held back.
//
// No report codec is involved -- the divergence is in channel selection.
func Test_Reports_SecondsResolutionDeterministicAcrossOptsCacheState(t *testing.T) {
	ctx := tests.Context(t)

	const channelID = llotypes.ChannelID(1)
	const seqNr = uint64(2)

	// A seconds-resolution channel whose validAfter and observation timestamp
	// collapse into the same second: the reports would overlap once truncated,
	// so the channel must not be reportable.
	cd := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked,
		Opts:         []byte(`{"TimeResolution":"s"}`),
	}

	outcome := Outcome{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: uint64(1000*time.Second + 900*time.Millisecond),
		ChannelDefinitions:              llotypes.ChannelDefinitions{channelID: cd},
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
			channelID: uint64(1000*time.Second + 100*time.Millisecond),
		},
	}

	// Deliberately register a codec that ignores opts entirely for this report
	// format. With the real EVM codec a cold oracle selects the channel and
	// then fails to encode it, so both oracles end up emitting nothing and the
	// selection divergence is masked. Removing the encoding step isolates it.
	newPlugin := func() *Plugin {
		p := newDeterminismTestPlugin(t)
		p.ReportCodecs = map[llotypes.ReportFormat]protocol.ReportCodec{
			llotypes.ReportFormatEVMABIEncodeUnpacked: reportcodec.JSONReportCodec{},
		}
		return p
	}

	warm := newPlugin()
	warm.OptsCache.ResetTo(outcome.ChannelDefinitions)
	cold := newPlugin()

	encoded, err := warm.OutcomeCodec.Encode(outcome)
	require.NoError(t, err)

	warmReports, err := warm.Reports(ctx, seqNr, encoded)
	require.NoError(t, err)
	coldReports, err := cold.Reports(ctx, seqNr, encoded)
	require.NoError(t, err)

	require.Empty(t, warmReports,
		"sanity check: a seconds-resolution channel must not be reported twice within the same second")
	assert.Equal(t, warmReports, coldReports,
		"channel selection must depend on the committed outcome alone, but the cold oracle reported a channel the warm oracle held back")
}

// Test_Reports_MemoizedOptsCacheDoesNotGoStale guards the risk introduced by
// memoizing opts decoding across rounds in a long-lived cache: a channel whose
// opts change must not keep encoding with the previous opts.
//
// The plugin is driven through two outcomes that differ only in one channel's
// opts, and each result is compared against a plugin that sees that outcome
// first, which cannot have memoized anything.
func Test_Reports_MemoizedOptsCacheDoesNotGoStale(t *testing.T) {
	ctx := tests.Context(t)

	const channelID = llotypes.ChannelID(1)
	const seqNr = uint64(2)

	optsWithFeedID := func(feedID string) []byte {
		return []byte(`{` +
			`"baseUSDFee":"0.1",` +
			`"expirationWindow":3600,` +
			`"feedID":"` + feedID + `",` +
			`"abi":[{"type":"int192","multiplier":"1"}],` +
			`"TimeResolution":"s"` +
			`}`)
	}

	outcomeFor := func(opts []byte) Outcome {
		return Outcome{
			LifeCycleStage:                  protocol.LifeCycleStageProduction,
			ObservationTimestampNanoseconds: uint64(2000 * time.Second),
			ChannelDefinitions: llotypes.ChannelDefinitions{channelID: {
				ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked,
				Streams: []llotypes.Stream{
					{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
					{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
					{StreamID: 3, Aggregator: llotypes.AggregatorMedian},
				},
				Opts: opts,
			}},
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{channelID: uint64(1999 * time.Second)},
			StreamAggregates: protocol.StreamAggregates{
				1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
				2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
				3: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(7))},
			},
		}
	}

	reportsFor := func(p *Plugin, outcome Outcome) []ocr3types.ReportPlus[llotypes.ReportInfo] {
		encoded, err := p.OutcomeCodec.Encode(outcome)
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, seqNr, encoded)
		require.NoError(t, err)
		return rwis
	}

	first := outcomeFor(optsWithFeedID("0x0003111111111111111111111111111111111111111111111111111111111111"))
	second := outcomeFor(optsWithFeedID("0x0003222222222222222222222222222222222222222222222222222222222222"))

	// Baselines from plugins that have never seen any other outcome.
	wantFirst := reportsFor(newDeterminismTestPlugin(t), first)
	wantSecond := reportsFor(newDeterminismTestPlugin(t), second)
	require.Len(t, wantFirst, 1)
	require.Len(t, wantSecond, 1)
	require.NotEqual(t, wantFirst, wantSecond, "sanity check: the two outcomes must produce different reports")

	// One plugin, both outcomes in sequence.
	reused := newDeterminismTestPlugin(t)
	assert.Equal(t, wantFirst, reportsFor(reused, first))
	assert.Equal(t, wantSecond, reportsFor(reused, second),
		"the memoized opts cache served stale opts after the channel definition changed")

	// And in the other order, so neither direction relies on ordering.
	reusedReverse := newDeterminismTestPlugin(t)
	assert.Equal(t, wantSecond, reportsFor(reusedReverse, second))
	assert.Equal(t, wantFirst, reportsFor(reusedReverse, first))
}
