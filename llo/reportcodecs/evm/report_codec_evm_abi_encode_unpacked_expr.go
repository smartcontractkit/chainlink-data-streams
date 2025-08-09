package evm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-data-streams/llo"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

var (
	_ llo.ReportCodec = ReportCodecEVMABIEncodeUnpackedExpr{}
)

type ReportCodecEVMABIEncodeUnpackedExpr struct {
	logger.Logger
	donID uint32
}

func NewReportCodecEVMABIEncodeUnpackedExpr(lggr logger.Logger, donID uint32) ReportCodecEVMABIEncodeUnpackedExpr {
	return ReportCodecEVMABIEncodeUnpackedExpr{logger.Sugared(lggr).Named("ReportCodecEVMABIEncodeUnpackedExpr"), donID}
}

// BaseReportFieldsNanoseconds represents the base report fields with nanosecond precision timestamps
type BaseReportFieldsNanoseconds struct {
	FeedID             common.Hash
	ValidFromTimestamp uint64
	Timestamp          uint64
	NativeFee          *big.Int
	LinkFee            *big.Int
	ExpiresAt          uint64
}

// BaseNanosecondSchema represents the fixed base schema for nanosecond timestamps
// that is used for EVMABIEncodeUnpackedExpr reports.
var BaseNanosecondSchema = getNanosecondBaseSchema()

func getNanosecondBaseSchema() abi.Arguments {
	mustNewType := func(t string) abi.Type {
		result, err := abi.NewType(t, "", []abi.ArgumentMarshaling{})
		if err != nil {
			panic(fmt.Sprintf("Unexpected error during abi.NewType: %s", err))
		}
		return result
	}
	return abi.Arguments([]abi.Argument{
		{Name: "feedId", Type: mustNewType("bytes32")},
		{Name: "validFromTimestamp", Type: mustNewType("uint64")},
		{Name: "observationsTimestamp", Type: mustNewType("uint64")},
		{Name: "nativeFee", Type: mustNewType("uint192")},
		{Name: "linkFee", Type: mustNewType("uint192")},
		{Name: "expiresAt", Type: mustNewType("uint64")},
	})
}

func (r ReportCodecEVMABIEncodeUnpackedExpr) Encode(report llo.Report, cd llotypes.ChannelDefinition) ([]byte, error) {
	if report.Specimen {
		return nil, errors.New("ReportCodecEVMABIEncodeUnpackedExpr does not support encoding specimen reports")
	}
	if len(report.Values) < 2 {
		return nil, fmt.Errorf("ReportCodecEVMABIEncodeUnpackedExpr requires at least 2 values (NativePrice, LinkPrice, ...); got report.Values: %v", report.Values)
	}
	nativePrice, err := extractPrice(report.Values[0])
	if err != nil {
		return nil, fmt.Errorf("ReportCodecEVMABIEncodeUnpackedExpr failed to extract native price: %w", err)
	}
	linkPrice, err := extractPrice(report.Values[1])
	if err != nil {
		return nil, fmt.Errorf("ReportCodecEVMABIEncodeUnpackedExpr failed to extract link price: %w", err)
	}

	// NOTE: It seems suboptimal to have to parse the opts on every encode but
	// not sure how to avoid it. Should be negligible performance hit as long
	// as Opts is small.
	opts := ReportFormatEVMABIEncodeOpts{}
	if err = (&opts).Decode(cd.Opts); err != nil {
		return nil, fmt.Errorf("failed to decode opts; got: '%s'; %w", cd.Opts, err)
	}

	rf := BaseReportFieldsNanoseconds{
		FeedID:             opts.FeedID,
		ValidFromTimestamp: report.ValidAfterNanoseconds + 1, // Add 1 nanosecond - smallest increment for nanosecond precision
		Timestamp:          report.ObservationTimestampNanoseconds,
		NativeFee:          CalculateFee(nativePrice, opts.BaseUSDFee),
		LinkFee:            CalculateFee(linkPrice, opts.BaseUSDFee),
		ExpiresAt:          report.ObservationTimestampNanoseconds + uint64(opts.ExpirationWindow)*1e9, // Convert seconds to nanoseconds
	}

	header, err := r.buildHeaderNanoseconds(rf)
	if err != nil {
		return nil, fmt.Errorf("failed to build base report; %w", err)
	}

	payload, err := buildPayload(opts.ABI, report.Values[len(report.Values)-len(opts.ABI):])
	if err != nil {
		return nil, fmt.Errorf("failed to build payload; %w", err)
	}

	return append(header, payload...), nil
}

func (r ReportCodecEVMABIEncodeUnpackedExpr) Verify(cd llotypes.ChannelDefinition) error {
	opts := new(ReportFormatEVMABIEncodeOpts)
	if err := opts.Decode(cd.Opts); err != nil {
		return fmt.Errorf("invalid Opts, got: %q; %w", cd.Opts, err)
	}
	if opts.BaseUSDFee.IsNegative() {
		return errors.New("baseUSDFee must be non-negative")
	}
	if opts.FeedID == (common.Hash{}) {
		return errors.New("feedID must not be zero")
	}
	if len(cd.Streams) < 3 {
		return fmt.Errorf("expected at least 3 streams; got: %d", len(cd.Streams))
	}
	return nil
}

func (r ReportCodecEVMABIEncodeUnpackedExpr) buildHeaderNanoseconds(rf BaseReportFieldsNanoseconds) ([]byte, error) {
	var merr error
	if rf.LinkFee == nil {
		merr = errors.Join(merr, errors.New("linkFee may not be nil"))
	} else if rf.LinkFee.Cmp(big.NewInt(0)) < 0 {
		merr = errors.Join(merr, fmt.Errorf("linkFee may not be negative (got: %s)", rf.LinkFee))
	}
	if rf.NativeFee == nil {
		merr = errors.Join(merr, errors.New("nativeFee may not be nil"))
	} else if rf.NativeFee.Cmp(big.NewInt(0)) < 0 {
		merr = errors.Join(merr, fmt.Errorf("nativeFee may not be negative (got: %s)", rf.NativeFee))
	}
	if merr != nil {
		return nil, merr
	}
	b, err := BaseNanosecondSchema.Pack(rf.FeedID, rf.ValidFromTimestamp, rf.Timestamp, rf.NativeFee, rf.LinkFee, rf.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("failed to pack base report blob; %w", err)
	}
	return b, nil
}
