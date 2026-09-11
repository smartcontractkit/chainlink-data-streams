package llo

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm"
)

// These tests pin down the invariant that the plugin's handling of a committed
// outcome must depend only on that outcome, never on node-local state.
//
// They currently FAIL, demonstrating the bug: channel Opts are part of the
// replicated outcome, but they are read back out of the node-local OptsCache,
// which is only ever populated by Outcome(). libocr can call Reports() on a
// committed outcome that the node never computed itself -- a restarted oracle
// catching up accepts a CertifiedCommit straight out of an epoch-start proof
// (outcome_generation_follower.go) and the report attestation loop calls
// Reports() on it -- so the empty-cache path is reachable in production.

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

// Test_Reports_DeterministicAcrossOptsCacheState asserts that two oracles given
// the same committed outcome return the same reports, regardless of whether
// they previously computed that outcome themselves.
//
// The report codec reads its options exclusively from the OptsCache
// (report_codec_evm_abi_encode_unpacked.go), so on a cold cache Encode fails,
// Reports() logs and skips the channel, and the two oracles disagree.
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

	// validAfter and the observation timestamp fall in different seconds, so
	// the channel is reportable under both seconds and nanosecond resolution.
	// That isolates the divergence to the encoding step.
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
	// An oracle that computed this outcome itself. This is exactly what
	// Outcome() leaves behind (see plugin_outcome.go, "Reset OptsCache").
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

// Test_ReportableChannels_DeterministicAcrossOptsCacheState covers the same
// root cause one layer down, without involving any report codec.
//
// IsSecondsResolution reads TimeResolution from the OptsCache and returns false
// on a miss, so a cold oracle skips the seconds-overlap check that a warm
// oracle applies. ReportableChannels feeds both Reports() and Outcome() (via
// IsReportable, which sets ValidAfterNanoseconds), so a disagreement here is a
// disagreement about the outcome itself, not only about reports.
func Test_ReportableChannels_DeterministicAcrossOptsCacheState(t *testing.T) {
	const channelID = llotypes.ChannelID(1)

	cd := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked,
		Opts:         []byte(`{"TimeResolution":"s"}`),
	}

	// validAfter and the observation timestamp collapse into the same second,
	// so a seconds-resolution channel must not be reportable.
	outcome := Outcome{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: uint64(1000*time.Second + 900*time.Millisecond),
		ChannelDefinitions:              llotypes.ChannelDefinitions{channelID: cd},
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
			channelID: uint64(1000*time.Second + 100*time.Millisecond),
		},
	}

	warmCache := protocol.NewOptsCache()
	warmCache.ResetTo(outcome.ChannelDefinitions)
	coldCache := protocol.NewOptsCache()

	warmReportable, _ := outcome.ReportableChannels(1, 0, warmCache)
	coldReportable, _ := outcome.ReportableChannels(1, 0, coldCache)

	require.Empty(t, warmReportable,
		"sanity check: a seconds-resolution channel must not be reportable twice within the same second")
	assert.Equal(t, warmReportable, coldReportable,
		"ReportableChannels() must be a function of the outcome alone, but the cold oracle disagreed")
}
