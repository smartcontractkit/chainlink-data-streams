package protocol

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/ubig"
)

// abiPayloadOpts reads the ABI of the formats that encode a payload from it.
type abiPayloadOpts struct {
	ABI []ABISpec `json:"abi"`
}

// premiumLegacyOpts reads the price multiplier of ReportFormatEVMPremiumLegacy.
type premiumLegacyOpts struct {
	Multiplier *ubig.Big `json:"multiplier"`
}

// ABIPayload returns the report values a format encodes in its ABI payload, in
// ABI order, and nil for a format without one. The codecs slice their payload
// through it, so the reportability check reads the same values they encode.
func ABIPayload(format llotypes.ReportFormat, abiLen int, values []StreamValue) ([]StreamValue, error) {
	switch format {
	case llotypes.ReportFormatEVMABIEncodeUnpacked:
		// Native and link prices come first.
		if len(values) < 2 || len(values)-2 != abiLen {
			return nil, fmt.Errorf("ABI and values length mismatch; ABI: %d, Values: %d", abiLen, len(values)-2)
		}
		return values[2:], nil
	case llotypes.ReportFormatEVMABIEncodeUnpackedExpr:
		// The trailing values are the calculated streams (see EffectiveStreams).
		if abiLen == 0 || len(values) < abiLen {
			return nil, fmt.Errorf("not enough values for calculated streams; expected at least: %d, got: %d", abiLen, len(values))
		}
		return values[len(values)-abiLen:], nil
	case llotypes.ReportFormatEVMStreamlined:
		if len(values) != abiLen {
			return nil, fmt.Errorf("ABI and values length mismatch; ABI: %d, Values: %d", abiLen, len(values))
		}
		return values, nil
	default:
		return nil, nil
	}
}

// CheckEncodableValues returns an error when a value of a report for cd falls
// outside the integer range its format encodes it in. Such a value depends on
// the data, so it can come back into range in a later round.
//
// It reads only cd's opts and the values, so it is deterministic and safe in the
// state transition. Formats without an integer encoding always pass.
func CheckEncodableValues(optsCache *OptsCache, cd llotypes.ChannelDefinition, channelID llotypes.ChannelID, values []StreamValue) error {
	switch cd.ReportFormat {
	case llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.ReportFormatEVMABIEncodeUnpackedExpr, llotypes.ReportFormatEVMStreamlined:
		o, err := GetOptsOrDecode[abiPayloadOpts](optsCache, cd, channelID)
		if err != nil {
			return fmt.Errorf("failed to decode ABI opts: %w", err)
		}
		payload, err := ABIPayload(cd.ReportFormat, len(o.ABI), values)
		if err != nil {
			return err
		}
		packed := cd.ReportFormat == llotypes.ReportFormatEVMStreamlined
		for i, spec := range o.ABI {
			if err := spec.CheckValue(payload[i], packed); err != nil {
				return fmt.Errorf("ABI index %d: %w", i, err)
			}
		}
		return nil
	case llotypes.ReportFormatEVMPremiumLegacy:
		return checkPremiumLegacyValues(optsCache, cd, channelID, values)
	default:
		return nil
	}
}

// checkPremiumLegacyValues checks the quote, which ReportFormatEVMPremiumLegacy
// encodes as int192 benchmark, bid and ask after the opts multiplier.
func checkPremiumLegacyValues(optsCache *OptsCache, cd llotypes.ChannelDefinition, channelID llotypes.ChannelID, values []StreamValue) error {
	if len(values) != 3 {
		return fmt.Errorf("expected exactly 3 values (NativePrice, LinkPrice, Quote); got: %d", len(values))
	}
	q, ok := values[2].(*Quote)
	if !ok || q == nil {
		return fmt.Errorf("expected third value to be a non-nil *Quote; got: %T", values[2])
	}
	o, err := GetOptsOrDecode[premiumLegacyOpts](optsCache, cd, channelID)
	if err != nil {
		return fmt.Errorf("failed to decode opts: %w", err)
	}
	if o.Multiplier != nil && o.Multiplier.IsZero() {
		return errors.New("multiplier, if specified in channel opts, must be non-zero")
	}
	price := ABIElement{Type: "int192", Multiplier: o.Multiplier}
	for _, f := range []struct {
		name string
		v    decimal.Decimal
	}{{"benchmark", q.Benchmark}, {"bid", q.Bid}, {"ask", q.Ask}} {
		if err := price.check(f.v, false); err != nil {
			return fmt.Errorf("quote %s: %w", f.name, err)
		}
	}
	return nil
}
