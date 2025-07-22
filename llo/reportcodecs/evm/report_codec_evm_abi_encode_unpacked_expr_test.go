package evm

import (
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"

	ubig "github.com/smartcontractkit/chainlink-data-streams/llo/reportcodecs/evm/utils"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-data-streams/llo"
)

func TestReportCodecEVMABIEncodeUnpackedExpr_Encode(t *testing.T) {
	t.Run("ABI and values length mismatch error", func(t *testing.T) {
		report := llo.Report{
			ConfigDigest:                    types.ConfigDigest{0x01},
			SeqNr:                           0x02,
			ChannelID:                       llotypes.ChannelID(0x03),
			ValidAfterNanoseconds:           0x04,
			ObservationTimestampNanoseconds: 0x05,
			Values: []llo.StreamValue{
				&llo.Quote{Bid: decimal.NewFromFloat(6.1), Benchmark: decimal.NewFromFloat(7.4), Ask: decimal.NewFromFloat(8.2332)},
				&llo.Quote{Bid: decimal.NewFromFloat(9.4), Benchmark: decimal.NewFromFloat(10.0), Ask: decimal.NewFromFloat(11.33)},
				llo.ToDecimal(decimal.NewFromFloat(100)),
				llo.ToDecimal(decimal.NewFromFloat(101)),
				llo.ToDecimal(decimal.NewFromFloat(102)),
			},
			Specimen: false,
		}

		opts := ReportFormatEVMABIEncodeOpts{
			ABI: []ABIEncoder{},
		}
		serializedOpts, err := opts.Encode()
		require.NoError(t, err)
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams: []llotypes.Stream{
				{
					Aggregator: llotypes.AggregatorMedian,
				},
				{
					Aggregator: llotypes.AggregatorMedian,
				},
				{
					Aggregator: llotypes.AggregatorQuote,
				},
				{
					Aggregator: llotypes.AggregatorMedian,
				},
				{
					Aggregator: llotypes.AggregatorMedian,
				},
				{
					Aggregator: llotypes.AggregatorMedian,
				},
			},
			Opts: serializedOpts,
		}

		codec := ReportCodecEVMABIEncodeUnpackedExpr{}
		_, err = codec.Encode(report, cd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ABI and values length mismatch")
	})

	t.Run("DEX-based asset schema example", func(t *testing.T) {
		expectedDEXBasedAssetSchema := abi.Arguments([]abi.Argument{
			{Name: "feedId", Type: mustNewABIType("bytes32")},
			{Name: "validFromTimestamp", Type: mustNewABIType("uint32")},
			{Name: "observationsTimestamp", Type: mustNewABIType("uint32")},
			{Name: "nativeFee", Type: mustNewABIType("uint192")},
			{Name: "linkFee", Type: mustNewABIType("uint192")},
			{Name: "expiresAt", Type: mustNewABIType("uint32")},
			{Name: "price", Type: mustNewABIType("int192")},
			{Name: "baseMarketDepth", Type: mustNewABIType("int192")},
			{Name: "quoteMarketDepth", Type: mustNewABIType("int192")},
		})

		properties := gopter.NewProperties(nil)

		runTest := func(sampleFeedID common.Hash, sampleObservationTimestampNanoseconds, sampleValidAfterNanoseconds uint64, sampleExpirationWindow uint32, priceMultiplier, marketDepthMultiplier *ubig.Big, sampleBaseUSDFee, sampleLinkBenchmarkPrice, sampleNativeBenchmarkPrice, sampleDexBasedAssetPrice, sampleBaseMarketDepth, sampleQuoteMarketDepth decimal.Decimal) bool {
			report := llo.Report{
				ConfigDigest:                    types.ConfigDigest{0x01},
				SeqNr:                           0x02,
				ChannelID:                       llotypes.ChannelID(0x03),
				ValidAfterNanoseconds:           sampleValidAfterNanoseconds,
				ObservationTimestampNanoseconds: sampleObservationTimestampNanoseconds,
				Values: []llo.StreamValue{
					&llo.Quote{Bid: decimal.NewFromFloat(6.1), Benchmark: sampleLinkBenchmarkPrice, Ask: decimal.NewFromFloat(8.2332)},  // Link price
					&llo.Quote{Bid: decimal.NewFromFloat(9.4), Benchmark: sampleNativeBenchmarkPrice, Ask: decimal.NewFromFloat(11.33)}, // Native price
					llo.ToDecimal(sampleDexBasedAssetPrice), // DEX-based asset price
					llo.ToDecimal(sampleBaseMarketDepth),    // Base market depth
					llo.ToDecimal(sampleQuoteMarketDepth),   // Quote market depth
				},
				Specimen: false,
			}

			opts := ReportFormatEVMABIEncodeOpts{
				BaseUSDFee:       sampleBaseUSDFee,
				ExpirationWindow: sampleExpirationWindow,
				FeedID:           sampleFeedID,
				ABI: []ABIEncoder{
					// benchmark price
					newSingleABIEncoder("int192", priceMultiplier),
					// base market depth
					newSingleABIEncoder("int192", marketDepthMultiplier),
					// quote market depth
					newSingleABIEncoder("int192", marketDepthMultiplier),
				},
			}
			serializedOpts, err := opts.Encode()
			require.NoError(t, err)

			cd := llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
				Streams: []llotypes.Stream{
					{
						Aggregator: llotypes.AggregatorMedian,
					},
					{
						Aggregator: llotypes.AggregatorMedian,
					},
					{
						Aggregator: llotypes.AggregatorQuote,
					},
					{
						Aggregator: llotypes.AggregatorMedian,
					},
					{
						Aggregator: llotypes.AggregatorMedian,
					},
				},
				Opts: serializedOpts,
			}

			codec := ReportCodecEVMABIEncodeUnpackedExpr{}
			encoded, err := codec.Encode(report, cd)
			require.NoError(t, err)

			values, err := expectedDEXBasedAssetSchema.Unpack(encoded)
			require.NoError(t, err)

			require.Len(t, values, len(expectedDEXBasedAssetSchema))

			// doesn't crash if values are nil
			for i := range report.Values {
				report.Values[i] = nil
			}
			_, err = codec.Encode(report, cd)
			require.Error(t, err)

			return true
		}

		properties.Property("Encodes values", prop.ForAll(
			runTest,
			genFeedID(),
			genObservationTimestampNanoseconds(),
			genValidAfterNanoseconds(),
			genExpirationWindow(),
			genMultiplier(),
			genMultiplier(),
			genBaseUSDFee(),
			genLinkBenchmarkPrice(),
			genNativeBenchmarkPrice(),
			genDexBasedAssetPrice(),
			genMarketDepth(),
			genMarketDepth(),
		))

		properties.TestingRun(t)
	})
}

func TestReportCodecEVMABIEncodeUnpackedExpr_Verify(t *testing.T) {
	c := ReportCodecEVMABIEncodeUnpackedExpr{}
	t.Run("unrecognized fields in opts", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         []byte(`{"unknown":"field"}`),
		}
		err := c.Verify(cd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown field")
	})
	t.Run("invalid opts", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         []byte(`"invalid"`),
		}
		err := c.Verify(cd)
		require.Error(t, err)
		require.EqualError(t, err, "invalid Opts, got: \"\\\"invalid\\\"\"; json: cannot unmarshal string into Go value of type evm.ReportFormatEVMABIEncodeOpts")
	})
	t.Run("negative BaseUSDFee", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         []byte(`{"baseUSDFee":"-1"}`),
		}
		err := c.Verify(cd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "baseUSDFee must be non-negative")
	})
	t.Run("zero feedID", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         []byte(`{"feedID":"0x0000000000000000000000000000000000000000000000000000000000000000"}`),
		}
		err := c.Verify(cd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "feedID must not be zero")
	})
	t.Run("missing feedID", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         []byte(`{}`),
		}
		err := c.Verify(cd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "feedID must not be zero")
	})
	t.Run("not enough streams", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams: []llotypes.Stream{
				{StreamID: 1},
				{StreamID: 2},
			},
			Opts: []byte(`{"ABI":[{"type":"int192"}],"feedID":"0x1111111111111111111111111111111111111111111111111111111111111111"}`),
		}
		err := c.Verify(cd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected at least 3 streams; got: 2")
	})
	t.Run("ABI length does not match streams length", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams: []llotypes.Stream{
				{StreamID: 1},
				{StreamID: 2},
				{StreamID: 3},
				{StreamID: 4},
			},
			Opts: []byte(`{"ABI":[{"type":"int192"}],"feedID":"0x1111111111111111111111111111111111111111111111111111111111111111"}`),
		}
		err := c.Verify(cd)
		require.NoError(t, err)
	})
	t.Run("invalid feedID", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         []byte(`{"baseUSDFee":"1","feedID":"0x"}`),
		}
		err := c.Verify(cd)
		require.Error(t, err)
		require.EqualError(t, err, "invalid Opts, got: \"{\\\"baseUSDFee\\\":\\\"1\\\",\\\"feedID\\\":\\\"0x\\\"}\"; hex string has length 0, want 64 for common.Hash")
	})
	t.Run("valid", func(t *testing.T) {
		cd := llotypes.ChannelDefinition{
			Streams: []llotypes.Stream{
				{StreamID: 1},
				{StreamID: 2},
				{StreamID: 3},
			},
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         []byte(`{"baseUSDFee":"1","feedID":"0x1111111111111111111111111111111111111111111111111111111111111111","ABI":[{"streamID":1,"type":"int192"}]}`),
		}
		err := c.Verify(cd)
		require.NoError(t, err)
	})
}

func genDexBasedAssetPrice() gopter.Gen {
	return gen.Float64Range(0, 1000000).Map(func(f float64) decimal.Decimal {
		return decimal.NewFromFloat(f)
	})
}

func genMarketDepth() gopter.Gen {
	return gen.Float64Range(0, 1000000).Map(func(f float64) decimal.Decimal {
		return decimal.NewFromFloat(f)
	})
}
