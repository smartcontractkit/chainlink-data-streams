package evm

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec"
	v3 "github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm/v3"
)

// Test_ReportTimeResolution_MatchesCodecEncode ties protocol.ReportTimeResolution
// to what each codec writes. The reportability guard truncates with the mapped
// resolution and the codec truncates on Encode; if the two disagree, a report
// can pass the guard and still encode with ValidFrom after Timestamp.
//
// The timestamps share a second but differ at every finer resolution, so a
// codec truncating to the wrong unit changes at least one encoded field.
func Test_ReportTimeResolution_MatchesCodecEncode(t *testing.T) {
	const (
		validAfterNs = 1_700_000_000_111_222_333
		obsTsNs      = 1_700_000_000_444_555_666
	)

	// encodedTimestamps returns the validAfter and observation timestamp a
	// codec wrote, in the codec's unit.
	type encodedTimestamps func(t *testing.T, encoded []byte) (validAfter, obsTs uint64)

	abiHeader := func(t *testing.T, encoded []byte) (uint64, uint64) {
		// Header words: feedID, validFromTimestamp, timestamp, nativeFee, linkFee, expiresAt.
		require.GreaterOrEqual(t, len(encoded), 6*32)
		validFrom := new(big.Int).SetBytes(encoded[32:64])
		ts := new(big.Int).SetBytes(encoded[64:96])
		require.True(t, validFrom.IsUint64() && ts.IsUint64())
		return validFrom.Uint64() - 1, ts.Uint64()
	}
	abiOpts := func(resolution string) []byte {
		return []byte(`{"baseUSDFee":"1","expirationWindow":60,"feedID":"0x0102030000000000000000000000000000000000000000000000000000000000",` + resolution + `"abi":[{"type":"int192"}]}`)
	}
	abiValues := []protocol.StreamValue{
		protocol.ToDecimal(decimal.NewFromInt(2)),
		protocol.ToDecimal(decimal.NewFromInt(3)),
		protocol.ToDecimal(decimal.NewFromInt(4)),
	}

	type codecCase struct {
		format   llotypes.ReportFormat
		opts     []byte
		codec    protocol.ReportCodec
		values   []protocol.StreamValue
		encoded  encodedTimestamps
		obsTsSet bool // whether the encoding carries the observation timestamp
	}
	cases := []codecCase{
		{
			format: llotypes.ReportFormatEVMPremiumLegacy,
			opts:   []byte(`{"baseUSDFee":"1","expirationWindow":60,"feedId":"0x0102030000000000000000000000000000000000000000000000000000000000","multiplier":10}`),
			codec:  ReportCodecPremiumLegacy{logger.Nop(), 1},
			values: newValidPremiumLegacyReport().Values,
			encoded: func(t *testing.T, encoded []byte) (uint64, uint64) {
				decoded, err := v3.Decode(encoded)
				require.NoError(t, err)
				return uint64(decoded.ValidFromTimestamp) - 1, uint64(decoded.ObservationsTimestamp)
			},
			obsTsSet: true,
		},
		{
			format: llotypes.ReportFormatJSON,
			codec:  reportcodec.JSONReportCodec{},
			values: abiValues,
			encoded: func(t *testing.T, encoded []byte) (uint64, uint64) {
				decoded, err := reportcodec.JSONReportCodec{}.Decode(encoded)
				require.NoError(t, err)
				return decoded.ValidAfterNanoseconds, decoded.ObservationTimestampNanoseconds
			},
			obsTsSet: true,
		},
		{
			format: llotypes.ReportFormatEVMStreamlined,
			opts:   []byte(`{"abi":[{"type":"int192"}]}`),
			codec:  ReportCodecEVMStreamlined{logger.Nop()},
			values: []protocol.StreamValue{protocol.ToDecimal(decimal.NewFromInt(2))},
			encoded: func(t *testing.T, encoded []byte) (uint64, uint64) {
				// Without a feed ID: report format, channel ID, then validAfter.
				require.GreaterOrEqual(t, len(encoded), 16)
				return binary.BigEndian.Uint64(encoded[8:16]), 0
			},
		},
	}
	for _, resolution := range []string{``, `"TimeResolution":"s",`, `"TimeResolution":"ms",`, `"TimeResolution":"us",`, `"TimeResolution":"ns",`} {
		cases = append(cases,
			codecCase{llotypes.ReportFormatEVMABIEncodeUnpacked, abiOpts(resolution), ReportCodecEVMABIEncodeUnpacked{logger.Nop(), 1}, abiValues, abiHeader, true},
			codecCase{llotypes.ReportFormatEVMABIEncodeUnpackedExpr, abiOpts(resolution), ReportCodecEVMABIEncodeUnpackedExpr{logger.Nop(), 1}, abiValues, abiHeader, true},
		)
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s %s", tc.format, tc.opts), func(t *testing.T) {
			cd := llotypes.ChannelDefinition{ReportFormat: tc.format, Opts: tc.opts}
			cache := protocol.NewOptsCache()
			cache.Set(1, cd.Opts)

			res, err := protocol.ReportTimeResolution(cache, cd, 1)
			require.NoError(t, err)

			encoded, err := tc.codec.Encode(protocol.Report{
				SeqNr:                           1,
				ChannelID:                       1,
				ValidAfterNanoseconds:           validAfterNs,
				ObservationTimestampNanoseconds: obsTsNs,
				Values:                          tc.values,
			}, cd, cache)
			require.NoError(t, err)

			gotValidAfter, gotObsTs := tc.encoded(t, encoded)
			require.Equal(t, protocol.ConvertTimestamp(validAfterNs, res), gotValidAfter, "validAfter truncation differs from the mapped resolution")
			if tc.obsTsSet {
				require.Equal(t, protocol.ConvertTimestamp(obsTsNs, res), gotObsTs, "observation timestamp truncation differs from the mapped resolution")
			}
		})
	}
}
