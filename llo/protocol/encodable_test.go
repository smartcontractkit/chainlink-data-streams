package protocol

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func TestABIPayload(t *testing.T) {
	v := func(n int64) StreamValue { return ToDecimal(decimal.NewFromInt(n)) }
	values := []StreamValue{v(1), v(2), v(3), v(4)}

	got, err := ABIPayload(llotypes.ReportFormatEVMABIEncodeUnpacked, 2, values)
	require.NoError(t, err)
	require.Equal(t, values[2:], got)
	_, err = ABIPayload(llotypes.ReportFormatEVMABIEncodeUnpacked, 3, values)
	require.ErrorContains(t, err, "length mismatch")
	_, err = ABIPayload(llotypes.ReportFormatEVMABIEncodeUnpacked, 0, values[:1])
	require.ErrorContains(t, err, "length mismatch")

	got, err = ABIPayload(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, 1, values)
	require.NoError(t, err)
	require.Equal(t, values[3:], got)
	_, err = ABIPayload(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, 5, values)
	require.ErrorContains(t, err, "not enough values")
	_, err = ABIPayload(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, 0, values)
	require.ErrorContains(t, err, "not enough values")

	got, err = ABIPayload(llotypes.ReportFormatEVMStreamlined, 4, values)
	require.NoError(t, err)
	require.Equal(t, values, got)
	_, err = ABIPayload(llotypes.ReportFormatEVMStreamlined, 3, values)
	require.ErrorContains(t, err, "length mismatch")

	got, err = ABIPayload(llotypes.ReportFormatJSON, 0, values)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestCheckEncodableValues(t *testing.T) {
	dec := func(s string) StreamValue { return ToDecimal(decimal.RequireFromString(s)) }
	quote := func(bid, benchmark, ask string) StreamValue {
		return &Quote{Bid: decimal.RequireFromString(bid), Benchmark: decimal.RequireFromString(benchmark), Ask: decimal.RequireFromString(ask)}
	}
	// 2^191, the first value past int192.
	const int192Overflow = "3138550867693340381917894711603833208051177722232017256448"

	for _, tc := range []struct {
		name   string
		format llotypes.ReportFormat
		opts   string
		values []StreamValue
		err    string
	}{
		{"unpacked in range", llotypes.ReportFormatEVMABIEncodeUnpacked, `{"abi":[{"type":"uint32","multiplier":"100"}]}`,
			[]StreamValue{dec("1"), dec("1"), dec("42.5")}, ""},
		{"unpacked negative for unsigned", llotypes.ReportFormatEVMABIEncodeUnpacked, `{"abi":[{"type":"uint32"}]}`,
			[]StreamValue{dec("1"), dec("1"), dec("-1")}, "ABI index 0: negative value"},
		{"unpacked over range after multiplier", llotypes.ReportFormatEVMABIEncodeUnpacked, `{"abi":[{"type":"uint32","multiplier":"1000000000"}]}`,
			[]StreamValue{dec("1"), dec("1"), dec("5")}, "out of range for type uint32"},
		{"unpacked native and link prices are not range checked", llotypes.ReportFormatEVMABIEncodeUnpacked, `{"abi":[{"type":"uint8"}]}`,
			[]StreamValue{dec("-1"), dec("1e100"), dec("1")}, ""},
		{"unpacked length mismatch", llotypes.ReportFormatEVMABIEncodeUnpacked, `{"abi":[{"type":"uint8"}]}`,
			[]StreamValue{dec("1"), dec("1")}, "length mismatch"},
		{"unpacked bytes0 is padded", llotypes.ReportFormatEVMABIEncodeUnpacked, `{"abi":[{"type":"bytes0"}]}`,
			[]StreamValue{dec("1"), dec("1"), dec("1")}, "invalid Solidity type"},
		{"unpacked opts do not decode", llotypes.ReportFormatEVMABIEncodeUnpacked, `{"abi":`,
			[]StreamValue{dec("1"), dec("1"), dec("1")}, "failed to decode ABI opts"},
		{"expr checks the trailing values", llotypes.ReportFormatEVMABIEncodeUnpackedExpr, `{"abi":[{"type":"int8","expression":"s1","expressionStreamID":9}]}`,
			[]StreamValue{dec("1"), dec("1"), dec("1000"), dec("-128")}, ""},
		{"expr out of range", llotypes.ReportFormatEVMABIEncodeUnpackedExpr, `{"abi":[{"type":"int8","expression":"s1","expressionStreamID":9}]}`,
			[]StreamValue{dec("1"), dec("1"), dec("-129")}, "out of range for type int8"},
		{"streamlined bytes0 is packed", llotypes.ReportFormatEVMStreamlined, `{"abi":[{"type":"bytes0"},{"type":"uint8"}]}`,
			[]StreamValue{dec("-1"), dec("255")}, ""},
		{"streamlined out of range", llotypes.ReportFormatEVMStreamlined, `{"abi":[{"type":"bytes0"},{"type":"uint8"}]}`,
			[]StreamValue{dec("-1"), dec("256")}, "ABI index 1"},
		{"legacy in range", llotypes.ReportFormatEVMPremiumLegacy, ``,
			[]StreamValue{dec("1"), dec("1"), quote("1", "2", "3")}, ""},
		{"legacy multiplier pushes past int192", llotypes.ReportFormatEVMPremiumLegacy, `{"multiplier":"1000000000000000000"}`,
			[]StreamValue{dec("1"), dec("1"), quote("1", "1e40", "3")}, "quote benchmark"},
		{"legacy bid past int192", llotypes.ReportFormatEVMPremiumLegacy, ``,
			[]StreamValue{dec("1"), dec("1"), quote(int192Overflow, "1", "1")}, "quote bid"},
		{"legacy zero multiplier", llotypes.ReportFormatEVMPremiumLegacy, `{"multiplier":"0"}`,
			[]StreamValue{dec("1"), dec("1"), quote("1", "1", "1")}, "must be non-zero"},
		{"legacy not a quote", llotypes.ReportFormatEVMPremiumLegacy, ``,
			[]StreamValue{dec("1"), dec("1"), dec("1")}, "non-nil *Quote"},
		{"json has no integer encoding", llotypes.ReportFormatJSON, ``,
			[]StreamValue{dec("-1e100")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cd := llotypes.ChannelDefinition{ReportFormat: tc.format, Opts: []byte(tc.opts)}
			cache := NewOptsCache()
			cache.ResetTo(llotypes.ChannelDefinitions{1: cd})
			for _, c := range []*OptsCache{cache, nil} {
				err := CheckEncodableValues(c, cd, 1, tc.values)
				if tc.err == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tc.err)
				}
			}
		})
	}
}
