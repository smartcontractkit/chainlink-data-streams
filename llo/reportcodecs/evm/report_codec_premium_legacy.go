package evm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2/chains/evmutil"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-data-streams/llo"
	ubig "github.com/smartcontractkit/chainlink-data-streams/llo/reportcodecs/evm/utils"
	v3 "github.com/smartcontractkit/chainlink-data-streams/llo/reportcodecs/evm/v3"
)

var (
	_            llo.ReportCodec = ReportCodecPremiumLegacy{}
	PayloadTypes                 = getPayloadTypes()
)

func getPayloadTypes() abi.Arguments {
	mustNewType := func(t string) abi.Type {
		result, err := abi.NewType(t, "", []abi.ArgumentMarshaling{})
		if err != nil {
			panic(fmt.Sprintf("Unexpected error during abi.NewType: %s", err))
		}
		return result
	}
	return abi.Arguments([]abi.Argument{
		{Name: "reportContext", Type: mustNewType("bytes32[3]")},
		{Name: "report", Type: mustNewType("bytes")},
		{Name: "rawRs", Type: mustNewType("bytes32[]")},
		{Name: "rawSs", Type: mustNewType("bytes32[]")},
		{Name: "rawVs", Type: mustNewType("bytes32")},
	})
}

type ReportCodecPremiumLegacy struct {
	logger.Logger
	donID uint32
}

func NewReportCodecPremiumLegacy(lggr logger.Logger, donID uint32) ReportCodecPremiumLegacy {
	return ReportCodecPremiumLegacy{logger.Sugared(lggr).Named("ReportCodecPremiumLegacy"), donID}
}

type ReportFormatEVMPremiumLegacyOpts struct {
	// BaseUSDFee is the cost on-chain of verifying a report
	BaseUSDFee decimal.Decimal `json:"baseUSDFee"`
	// Expiration window is the length of time in seconds the report is valid
	// for, from the observation timestamp
	ExpirationWindow uint32 `json:"expirationWindow"`
	// FeedID is for compatibility with existing on-chain verifiers
	FeedID common.Hash `json:"feedID"`
	// Multiplier is used to scale the bid, benchmark and ask values in the
	// report. If not specified, or zero is used, a multiplier of 1 is assumed.
	Multiplier *ubig.Big `json:"multiplier"`
}

func (r *ReportFormatEVMPremiumLegacyOpts) Decode(opts []byte) error {
	if len(opts) == 0 {
		// special case if opts are unspecified, just use the zero options rather than erroring
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(opts))
	decoder.DisallowUnknownFields() // Error on unrecognized fields
	return decoder.Decode(r)
}

func (r ReportCodecPremiumLegacy) Encode(report llo.Report, cd llotypes.ChannelDefinition) ([]byte, error) {
	if report.Specimen {
		return nil, errors.New("ReportCodecPremiumLegacy does not support encoding specimen reports")
	}
	nativePrice, linkPrice, quote, err := ExtractReportValues(report)
	if err != nil {
		return nil, fmt.Errorf("ReportCodecPremiumLegacy cannot encode; got unusable report; %w", err)
	}

	// NOTE: It seems suboptimal to have to parse the opts on every encode but
	// not sure how to avoid it. Should be negligible performance hit as long
	// as Opts is small.
	opts := ReportFormatEVMPremiumLegacyOpts{}
	if err = (&opts).Decode(cd.Opts); err != nil {
		return nil, fmt.Errorf("failed to decode opts; got: '%s'; %w", cd.Opts, err)
	}
	var multiplier decimal.Decimal
	if opts.Multiplier == nil {
		multiplier = decimal.NewFromInt(1)
	} else if opts.Multiplier.IsZero() {
		return nil, errors.New("multiplier, if specified in channel opts, must be non-zero")
	} else {
		multiplier = decimal.NewFromBigInt(opts.Multiplier.ToInt(), 0)
	}

	validAfterSeconds, observationTimestampSeconds, err := ExtractTimestamps(report)
	if err != nil {
		return nil, fmt.Errorf("failed to extract timestamps; %w", err)
	}

	rf := v3.ReportFields{
		ValidFromTimestamp: validAfterSeconds + 1,
		Timestamp:          observationTimestampSeconds,
		NativeFee:          CalculateFee(nativePrice, opts.BaseUSDFee),
		LinkFee:            CalculateFee(linkPrice, opts.BaseUSDFee),
		ExpiresAt:          observationTimestampSeconds + opts.ExpirationWindow,
		BenchmarkPrice:     quote.Benchmark.Mul(multiplier).BigInt(),
		Bid:                quote.Bid.Mul(multiplier).BigInt(),
		Ask:                quote.Ask.Mul(multiplier).BigInt(),
	}

	r.Logger.Debugw("Encoding report", "report", report, "opts", opts, "nativePrice", nativePrice, "linkPrice", linkPrice, "quote", quote, "multiplier", multiplier, "rf", rf)

	codec := v3.NewReportCodec(opts.FeedID, r.Logger)
	return codec.BuildReport(rf)
}

func (r ReportCodecPremiumLegacy) Verify(cd llotypes.ChannelDefinition) error {
	opts := ReportFormatEVMPremiumLegacyOpts{}
	if err := (&opts).Decode(cd.Opts); err != nil {
		return fmt.Errorf("invalid Opts, got: %q; %w", cd.Opts, err)
	}
	if opts.BaseUSDFee.IsNegative() {
		return errors.New("baseUSDFee must be non-negative")
	}
	if opts.FeedID == (common.Hash{}) {
		return errors.New("feedID must not be zero")
	}
	if len(cd.Streams) != 3 {
		return fmt.Errorf("ReportFormatEVMPremiumLegacy requires exactly 3 streams (NativePrice, LinkPrice, Quote); got: %v", cd.Streams)
	}
	return nil
}

func (r ReportCodecPremiumLegacy) Decode(b []byte) (*v3.Report, error) {
	codec := v3.NewReportCodec([32]byte{}, r.Logger)
	return codec.Decode(b)
}

// Pack assembles the report values into a payload for verifying on-chain
func (r ReportCodecPremiumLegacy) Pack(digest types.ConfigDigest, seqNr uint64, report ocr2types.Report, sigs []types.AttributedOnchainSignature) ([]byte, error) {
	var rs [][32]byte
	var ss [][32]byte
	var vs [32]byte
	for i, as := range sigs {
		r, s, v, err := evmutil.SplitSignature(as.Signature) //nolint:revive // This has always worked before; no need to change it
		if err != nil {
			return nil, fmt.Errorf("eventTransmit(ev): error in SplitSignature: %w", err)
		}
		rs = append(rs, r)
		ss = append(ss, s)
		vs[i] = v
	}
	reportCtx, err := LegacyReportContext(digest, seqNr, r.donID)
	if err != nil {
		return nil, fmt.Errorf("failed to get legacy report context: %w", err)
	}
	rawReportCtx := evmutil.RawReportContext(reportCtx)

	payload, err := PayloadTypes.Pack(rawReportCtx, []byte(report), rs, ss, vs)
	if err != nil {
		return nil, fmt.Errorf("abi.Pack failed; %w", err)
	}
	return payload, nil
}

// ExtractReportValues extracts the native price, link price and quote from the report
// Can handle either *Decimal or *Quote types for native/link prices
func ExtractReportValues(report llo.Report) (nativePrice, linkPrice decimal.Decimal, quote *llo.Quote, err error) {
	if len(report.Values) != 3 {
		err = fmt.Errorf("ReportCodecPremiumLegacy requires exactly 3 values (NativePrice, LinkPrice, Quote{Bid, Mid, Ask}); got report.Values: %v", report.Values)
		return
	}
	nativePrice, err = extractPrice(report.Values[0])
	if err != nil {
		err = fmt.Errorf("ReportCodecPremiumLegacy failed to extract native price: %w", err)
		return
	}
	linkPrice, err = extractPrice(report.Values[1])
	if err != nil {
		err = fmt.Errorf("ReportCodecPremiumLegacy failed to extract link price: %w", err)
		return
	}
	var is bool
	quote, is = report.Values[2].(*llo.Quote)
	if !is {
		err = fmt.Errorf("ReportCodecPremiumLegacy expects third stream value to be of type *Quote; got: %T", report.Values[2])
		return
	}
	if quote == nil {
		err = errors.New("ReportCodecPremiumLegacy expects third stream value to be non-nil")
		return
	}
	return nativePrice, linkPrice, quote, nil
}

func extractPrice(price llo.StreamValue) (decimal.Decimal, error) {
	switch p := price.(type) {
	case *llo.Decimal:
		if p == nil {
			// Missing price will cause a zero fee
			return decimal.Zero, nil
		}
		return p.Decimal(), nil
	case *llo.Quote:
		// in case of quote feed, use the benchmark price
		if p == nil {
			return decimal.Zero, nil
		}
		return p.Benchmark, nil

	case nil:
		return decimal.Zero, nil
	default:
		return decimal.Zero, fmt.Errorf("expected *Decimal or *Quote; got: %T", price)
	}
}

const PluginVersion uint32 = 1 // the legacy mercury plugin is 0

// Uniquely identifies this as LLO plugin, rather than the legacy plugin (which
// uses all zeroes).
//
// This is quite a hack but serves the purpose of uniquely identifying
// dons/plugin versions to the mercury server without having to modify any
// existing tooling or breaking backwards compatibility. It should be safe
// since the DonID is encoded into the config digest anyway so report context
// is already dependent on it, and all LLO jobs in the same don are expected to
// have the same don ID set.
//
// Packs donID+pluginVersion as (uint32, uint32), for example donID=2,
// PluginVersion=1 Yields:
// 0x0000000000000000000000000000000000000000000000000000000200000001
func LLOExtraHash(donID uint32) common.Hash {
	combined := uint64(donID)<<32 | uint64(PluginVersion)
	return common.BigToHash(new(big.Int).SetUint64(combined))
}

func SeqNrToEpochAndRound(seqNr uint64) (epoch uint32, round uint8, err error) {
	// Simulate 256 rounds/epoch
	if seqNr/256 > math.MaxUint32 {
		err = fmt.Errorf("epoch overflows uint32: %d", seqNr)
		return
	}
	epoch = uint32(seqNr / 256) //nolint:gosec // G115 false positive
	round = uint8(seqNr % 256)  //nolint:gosec // G115 false positive
	return
}

func LegacyReportContext(cd ocr2types.ConfigDigest, seqNr uint64, donID uint32) (ocr2types.ReportContext, error) {
	epoch, round, err := SeqNrToEpochAndRound(seqNr)
	if err != nil {
		return ocr2types.ReportContext{}, err
	}
	return ocr2types.ReportContext{
		ReportTimestamp: ocr2types.ReportTimestamp{
			ConfigDigest: cd,
			Epoch:        epoch,
			Round:        round,
		},
		ExtraHash: LLOExtraHash(donID), // ExtraHash is always zero for mercury, we use LLOExtraHash here to differentiate from the legacy plugin
	}, nil
}
