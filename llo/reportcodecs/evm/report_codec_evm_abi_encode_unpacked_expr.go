package evm

import (
	"errors"
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/goccy/go-json"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-data-streams/llo"
)

var (
	_ llo.ReportCodec = ReportCodecEVMABIEncodeUnpackedExpr{}
	_ llo.OptsParser  = ReportCodecEVMABIEncodeUnpackedExpr{}
)

type ReportCodecEVMABIEncodeUnpackedExpr struct {
	logger.Logger
	donID uint32
}

func NewReportCodecEVMABIEncodeUnpackedExpr(lggr logger.Logger, donID uint32) ReportCodecEVMABIEncodeUnpackedExpr {
	return ReportCodecEVMABIEncodeUnpackedExpr{logger.Sugared(lggr).Named("ReportCodecEVMABIEncodeUnpackedExpr"), donID}
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

	validAfter := ConvertTimestamp(report.ValidAfterNanoseconds, opts.TimeResolution)
	observationTimestamp := ConvertTimestamp(report.ObservationTimestampNanoseconds, opts.TimeResolution)

	rf := BaseReportFields{
		FeedID:             opts.FeedID,
		ValidFromTimestamp: validAfter + 1,
		Timestamp:          observationTimestamp,
		NativeFee:          CalculateFee(nativePrice, opts.BaseUSDFee),
		LinkFee:            CalculateFee(linkPrice, opts.BaseUSDFee),
		ExpiresAt:          observationTimestamp + uint64(opts.ExpirationWindow),
	}

	header, err := r.buildHeader(rf, opts.TimeResolution)
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

func (r ReportCodecEVMABIEncodeUnpackedExpr) buildHeader(rf BaseReportFields, precision llo.TimeResolution) ([]byte, error) {
	var merr error
	if rf.LinkFee == nil {
		merr = errors.Join(merr, errors.New("linkFee may not be nil"))
	} else if rf.LinkFee.Cmp(zero) < 0 {
		merr = errors.Join(merr, fmt.Errorf("linkFee may not be negative (got: %s)", rf.LinkFee))
	}
	if rf.NativeFee == nil {
		merr = errors.Join(merr, errors.New("nativeFee may not be nil"))
	} else if rf.NativeFee.Cmp(zero) < 0 {
		merr = errors.Join(merr, fmt.Errorf("nativeFee may not be negative (got: %s)", rf.NativeFee))
	}
	if merr != nil {
		return nil, merr
	}

	var b []byte
	var err error
	if precision == llo.ResolutionSeconds {
		if rf.ValidFromTimestamp > math.MaxUint32 {
			return nil, fmt.Errorf("validFromTimestamp %d exceeds uint32 range", rf.ValidFromTimestamp)
		}
		if rf.Timestamp > math.MaxUint32 {
			return nil, fmt.Errorf("timestamp %d exceeds uint32 range", rf.Timestamp)
		}
		if rf.ExpiresAt > math.MaxUint32 {
			return nil, fmt.Errorf("expiresAt %d exceeds uint32 range", rf.ExpiresAt)
		}
		b, err = BaseSchemaUint32.Pack(
			rf.FeedID,
			uint32(rf.ValidFromTimestamp),
			uint32(rf.Timestamp),
			rf.NativeFee,
			rf.LinkFee,
			uint32(rf.ExpiresAt),
		)
	} else {
		b, err = BaseSchemaUint64.Pack(
			rf.FeedID,
			rf.ValidFromTimestamp,
			rf.Timestamp,
			rf.NativeFee,
			rf.LinkFee,
			rf.ExpiresAt,
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to pack base report blob; %w", err)
	}
	return b, nil
}

func (r ReportCodecEVMABIEncodeUnpackedExpr) ParseOpts(opts []byte) (interface{}, error) {
	var o ReportFormatEVMABIEncodeOpts
	if err := json.Unmarshal(opts, &o); err != nil {
		return nil, fmt.Errorf("failed to parse EVMABIEncodeUnpackedExpr opts: %w", err)
	}
	return o, nil
}

func (r ReportCodecEVMABIEncodeUnpackedExpr) TimeResolution(parsedOpts interface{}) (llo.TimeResolution, error) {
	opts, ok := parsedOpts.(ReportFormatEVMABIEncodeOpts)
	if !ok {
		return llo.ResolutionSeconds, fmt.Errorf("expected ReportFormatEVMABIEncodeOpts, got %T", parsedOpts)
	}
	return opts.TimeResolution, nil
}
