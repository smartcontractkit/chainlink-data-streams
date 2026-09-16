package llo

import (
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// Test_decodeObservation_CoefficientBound covers the blob-carried path, which is
// how v31 observations carry stream values at all: a value over the coefficient
// bound must be rejected there, not merely in the protocol-level decoder.
//
// The rejection is a plain error rather than a blobFetchError, which is what
// makes it deterministic across oracles and therefore safe for
// decodeObservations to drop this observation alone instead of failing the round.
func Test_decodeObservation_CoefficientBound(t *testing.T) {
	ctx := tests.Context(t)

	overSized := decimal.NewFromBigInt(new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits), -2)
	atLimit := decimal.NewFromBigInt(new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits-1), -2)

	encode := func(t *testing.T, d decimal.Decimal) []byte {
		return mustEncodeObs(t, Observation{
			UnixTimestampNanoseconds: 1,
			StreamValues:             protocol.StreamValues{1: protocol.ToDecimal(d)},
		})
	}

	obs, err := decodeObservation(ctx, encode(t, atLimit), testBlobs)
	require.NoError(t, err)
	require.Len(t, obs.StreamValues, 1)

	_, err = decodeObservation(ctx, encode(t, overSized), testBlobs)
	require.ErrorIs(t, err, protocol.ErrDecimalCoefficientOutOfRange)

	var bfErr *blobFetchError
	require.NotErrorAs(t, err, &bfErr,
		"must be a deterministic error so the observation is dropped, not the round")
}

// Test_StateTransition_DropsObservationOverCoefficientBound is the consequence
// that matters: an oracle sending an unbounded value costs its own observation
// and nothing else. The round completes and the remaining observations still
// aggregate, which is why this bound is safe to enforce on the consensus path.
func Test_StateTransition_DropsObservationOverCoefficientBound(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	channel := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
	}

	// Bootstrap, then install the channel so it is effective for the round below.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil), ao(3, nil)}, kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1_000, 100, channel), kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, addChannelRound(t, 2_000, 100, channel), kv, testBlobs)
	require.NoError(t, err)

	good := decimal.NewFromInt(42)
	overSized := decimal.NewFromBigInt(new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits), -2)

	obsWith := func(d decimal.Decimal) []byte {
		return mustEncodeObs(t, Observation{
			UnixTimestampNanoseconds: 3_000,
			StreamValues:             protocol.StreamValues{1: protocol.ToDecimal(d)},
		})
	}
	// Three honest observations and one unbounded, which is f=1 for this plugin.
	aos := []ocrtypes.AttributedObservation{
		ao(0, obsWith(good)),
		ao(1, obsWith(good)),
		ao(2, obsWith(good)),
		ao(3, obsWith(overSized)),
	}

	precBytes, err := p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
	require.NoError(t, err, "one unbounded observation must not fail the round")

	prec, err := decodePrecursor(precBytes)
	require.NoError(t, err)
	aggregated, ok := prec.StreamAggregates[1][llotypes.AggregatorMedian]
	require.True(t, ok, "the honest observations must still aggregate")
	require.Equal(t, good.String(), mustText(t, aggregated))
}

func mustText(t *testing.T, sv protocol.StreamValue) string {
	t.Helper()
	b, err := sv.MarshalText()
	require.NoError(t, err)
	return string(b)
}
