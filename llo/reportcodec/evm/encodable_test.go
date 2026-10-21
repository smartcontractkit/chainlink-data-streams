package evm

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	v3 "github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm/v3"
)

const testFeedID = `"0x0001000000000000000000000000000000000000000000000000000000000001"`

// The reportability check and Encode must agree: a value the check accepts must
// encode, and one it rejects must fail Encode, or the watermark advances over a
// round that emitted nothing.
func TestCheckEncodableValues_AgreesWithEncode(t *testing.T) {
	lggr := logger.Nop()
	dec := func(s string) protocol.StreamValue { return protocol.ToDecimal(decimal.RequireFromString(s)) }
	quote := func(benchmark string) protocol.StreamValue {
		return &protocol.Quote{Bid: decimal.NewFromInt(1), Benchmark: decimal.RequireFromString(benchmark), Ask: decimal.NewFromInt(1)}
	}
	ts := func(observedAt uint64, v string) protocol.StreamValue {
		return &protocol.TimestampedStreamValue{ObservedAtNanoseconds: observedAt, StreamValue: dec(v)}
	}
	unpackedOpts := func(abi string) string {
		return `{"baseUSDFee":"1","expirationWindow":3600,"feedID":` + testFeedID + `,"abi":` + abi + `}`
	}
	payloads := []protocol.StreamValue{
		dec("0"), dec("127"), dec("127.9"), dec("128"), dec("-128"), dec("-129"), dec("255"), dec("256"), dec("-1"),
		dec("1e80"), dec("-1e80"), dec("0.001"),
		ts(1_700_000_000_000_000_000, "1"), ts(1, "-129"), &protocol.Quote{},
	}

	for _, tc := range []struct {
		name   string
		format llotypes.ReportFormat
		codec  protocol.ReportCodec
		opts   []string
		values func(v protocol.StreamValue) []protocol.StreamValue
		inputs []protocol.StreamValue
	}{
		{
			name:   "unpacked",
			format: llotypes.ReportFormatEVMABIEncodeUnpacked,
			codec:  NewReportCodecEVMABIEncodeUnpacked(lggr, 1),
			opts: []string{
				unpackedOpts(`[{"type":"int8"}]`), unpackedOpts(`[{"type":"uint8"}]`),
				unpackedOpts(`[{"type":"uint8","multiplier":"10"}]`), unpackedOpts(`[{"type":"int256","multiplier":"-1"}]`),
				unpackedOpts(`[[{"type":"uint64"},{"type":"int8"}]]`), unpackedOpts(`[[{"type":"uint32"},{"type":"int8"}]]`),
				unpackedOpts(`[{"type":"bytes0"}]`), unpackedOpts(`[{"type":"bool"}]`),
			},
			values: func(v protocol.StreamValue) []protocol.StreamValue {
				return []protocol.StreamValue{dec("2"), dec("3"), v}
			},
			inputs: payloads,
		},
		{
			name:   "expr",
			format: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			codec:  NewReportCodecEVMABIEncodeUnpackedExpr(lggr, 1),
			opts: []string{
				`{"baseUSDFee":"1","expirationWindow":3600,"feedID":` + testFeedID + `,"abi":[{"type":"int8","expression":"s1","expressionStreamID":9}]}`,
				`{"baseUSDFee":"1","expirationWindow":3600,"feedID":` + testFeedID + `,"abi":[{"type":"uint8","multiplier":"10","expression":"s1","expressionStreamID":9}]}`,
			},
			values: func(v protocol.StreamValue) []protocol.StreamValue {
				return []protocol.StreamValue{dec("2"), dec("3"), dec("1e90"), v}
			},
			inputs: payloads,
		},
		{
			name:   "streamlined",
			format: llotypes.ReportFormatEVMStreamlined,
			codec:  NewReportCodecStreamlined(lggr),
			opts: []string{
				`{"abi":[{"type":"int8"},{"type":"uint8"}]}`, `{"abi":[{"type":"bytes0"},{"type":"uint8"}]}`,
				`{"abi":[{"type":"int8"},[{"type":"uint64"},{"type":"int8"}]]}`, `{"abi":[{"type":"int8"},[{"type":"bytes0"},{"type":"uint8"}]]}`,
			},
			values: func(v protocol.StreamValue) []protocol.StreamValue { return []protocol.StreamValue{v, v} },
			inputs: payloads,
		},
		{
			name:   "premium legacy",
			format: llotypes.ReportFormatEVMPremiumLegacy,
			codec:  NewReportCodecPremiumLegacy(lggr, 1),
			opts: []string{
				`{"baseUSDFee":"1","expirationWindow":3600,"feedID":` + testFeedID + `}`,
				`{"baseUSDFee":"1","expirationWindow":3600,"feedID":` + testFeedID + `,"multiplier":"1000000000000000000"}`,
				`{"baseUSDFee":"1","expirationWindow":3600,"feedID":` + testFeedID + `,"multiplier":"0"}`,
			},
			values: func(v protocol.StreamValue) []protocol.StreamValue {
				return []protocol.StreamValue{dec("2"), dec("3"), v}
			},
			inputs: []protocol.StreamValue{
				quote("1"), quote("-1"), quote("1e38"), quote("1e39"), quote("-1e39"), quote("1e57"), quote("1e58"), dec("1"),
			},
		},
	} {
		for _, opts := range tc.opts {
			for _, in := range tc.inputs {
				values := tc.values(in)
				name := fmt.Sprintf("%s/%s/%v", tc.name, opts, in)
				t.Run(name, func(t *testing.T) {
					cd := llotypes.ChannelDefinition{ReportFormat: tc.format, Opts: []byte(opts)}
					cache := protocol.NewOptsCache()
					cache.ResetTo(llotypes.ChannelDefinitions{1: cd})
					checkErr := protocol.CheckEncodableValues(cache, cd, 1, values)
					_, encErr := tc.codec.Encode(protocol.Report{
						ChannelID:                       1,
						ValidAfterNanoseconds:           1_700_000_000_000_000_000,
						ObservationTimestampNanoseconds: 1_700_000_001_000_000_000,
						Values:                          values,
					}, cd, cache)
					require.Equal(t, checkErr == nil, encErr == nil, "check: %v, encode: %v", checkErr, encErr)
				})
			}
		}
	}
}

func TestVerifyForAdmission(t *testing.T) {
	lggr := logger.Nop()
	for _, tc := range []struct {
		name  string
		codec protocol.AdmissionVerifier
		opts  string
		err   string
	}{
		{"unpacked ok", NewReportCodecEVMABIEncodeUnpacked(lggr, 1), `{"expirationWindow":31536000,"abi":[{"type":"int192"},[{"type":"uint64"},{"type":"int192"}]]}`, ""},
		{"unpacked bytes0", NewReportCodecEVMABIEncodeUnpacked(lggr, 1), `{"abi":[{"type":"bytes0"}]}`, "abi[0][0]: invalid Solidity type: bytes0"},
		{"unpacked non integer type", NewReportCodecEVMABIEncodeUnpacked(lggr, 1), `{"abi":[[{"type":"uint64"},{"type":"string"}]]}`, "abi[0][1]: invalid Solidity type: string"},
		{"unpacked expiration window", NewReportCodecEVMABIEncodeUnpacked(lggr, 1), `{"expirationWindow":31536001,"abi":[{"type":"int192"}]}`, "expirationWindow must not exceed"},
		{"expr bytes0", NewReportCodecEVMABIEncodeUnpackedExpr(lggr, 1), `{"abi":[{"type":"bytes0","expression":"s1","expressionStreamID":9}]}`, "invalid Solidity type: bytes0"},
		{"expr expiration window", NewReportCodecEVMABIEncodeUnpackedExpr(lggr, 1), `{"expirationWindow":4000000000,"abi":[{"type":"int192","expression":"s1","expressionStreamID":9}]}`, "expirationWindow must not exceed"},
		{"streamlined bytes0 ok", NewReportCodecStreamlined(lggr), `{"abi":[{"type":"bytes0"},{"type":"uint8"}]}`, ""},
		{"streamlined non integer type", NewReportCodecStreamlined(lggr), `{"abi":[{"type":"bool"}]}`, "invalid Solidity type: bool"},
		{"streamlined fits uint16 length", NewReportCodecStreamlined(lggr), streamlinedOpts(2047, false), ""},
		{"streamlined exceeds uint16 length", NewReportCodecStreamlined(lggr), streamlinedOpts(2048, false), "exceeds maximum size"},
		{"streamlined feed ID header", NewReportCodecStreamlined(lggr), streamlinedOpts(2047, true), "exceeds maximum size"},
		{"legacy ok", NewReportCodecPremiumLegacy(lggr, 1), `{"expirationWindow":3600,"multiplier":"100"}`, ""},
		{"legacy zero multiplier", NewReportCodecPremiumLegacy(lggr, 1), `{"multiplier":"0"}`, "must be non-zero"},
		{"legacy expiration window", NewReportCodecPremiumLegacy(lggr, 1), `{"expirationWindow":4000000000}`, "expirationWindow must not exceed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Expressions read stream 1.
			err := tc.codec.VerifyForAdmission(llotypes.ChannelDefinition{
				Opts:    []byte(tc.opts),
				Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			})
			if tc.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.err)
			}
		})
	}
}

// streamlinedOpts returns opts with n uint256 elements. Without a feed ID the
// header is 16 bytes, so 2047 elements make 65520 bytes and 2048 make 65552.
func streamlinedOpts(n int, feedID bool) string {
	elems := make([]string, n)
	for i := range elems {
		elems[i] = `{"type":"uint256"}`
	}
	fid := ""
	if feedID {
		fid = `"feedID":` + testFeedID + `,`
	}
	return `{` + fid + `"abi":[` + strings.Join(elems, ",") + `]}`
}

// abi.Pack does not range check, so BuildReport must, or an out of range price
// or fee would be encoded as a different value.
func TestV3BuildReport_RejectsOutOfRange(t *testing.T) {
	codec := v3.NewReportCodec([32]byte{1}, logger.Nop())
	pow2 := func(n uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), n) }
	one := big.NewInt(1)
	ok := v3.ReportFields{BenchmarkPrice: one, Bid: one, Ask: one, NativeFee: one, LinkFee: one}

	_, err := codec.BuildReport(ok)
	require.NoError(t, err)

	rf := ok
	rf.BenchmarkPrice = pow2(191)
	_, err = codec.BuildReport(rf)
	require.ErrorContains(t, err, "benchmarkPrice: value")

	rf = ok
	rf.Ask = new(big.Int).Neg(pow2(200))
	_, err = codec.BuildReport(rf)
	require.ErrorContains(t, err, "ask: value")

	rf = ok
	rf.LinkFee = pow2(192)
	_, err = codec.BuildReport(rf)
	require.ErrorContains(t, err, "linkFee: value")
}

func TestCheckFee(t *testing.T) {
	require.NoError(t, checkFee("linkFee", big.NewInt(0)))
	require.ErrorContains(t, checkFee("linkFee", nil), "linkFee may not be nil")
	require.ErrorContains(t, checkFee("linkFee", big.NewInt(-1)), "linkFee may not be negative")
	require.ErrorContains(t, checkFee("nativeFee", new(big.Int).Lsh(big.NewInt(1), 192)), "nativeFee: value")
}
