package evm

import (
	"errors"
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"
)

var (
	_ protocol.ReportCodec       = ReportCodecEVMABIEncodeUnpackedExpr{}
	_ protocol.FeedIDer          = ReportCodecEVMABIEncodeUnpackedExpr{}
	_ protocol.AdmissionVerifier = ReportCodecEVMABIEncodeUnpackedExpr{}
)

type ReportCodecEVMABIEncodeUnpackedExpr struct {
	logger.Logger
	donID uint32
}

func NewReportCodecEVMABIEncodeUnpackedExpr(lggr logger.Logger, donID uint32) ReportCodecEVMABIEncodeUnpackedExpr {
	return ReportCodecEVMABIEncodeUnpackedExpr{logger.Sugared(lggr).Named("ReportCodecEVMABIEncodeUnpackedExpr"), donID}
}

func (r ReportCodecEVMABIEncodeUnpackedExpr) Encode(report protocol.Report, cd llotypes.ChannelDefinition, optsCache *protocol.OptsCache) ([]byte, error) {
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

	opts, getErr := protocol.GetOpts[ReportFormatEVMABIEncodeOpts](optsCache, report.ChannelID)
	if getErr != nil {
		return nil, fmt.Errorf("opts not in cache for channel %d: %w", report.ChannelID, getErr)
	}

	if len(opts.ABI) < 1 {
		return nil, fmt.Errorf("ReportCodecEVMABIEncodeUnpackedExpr no expressions found in channel definition")
	}

	// The payload is the trailing len(opts.ABI) values, one per declared
	// calculated stream. protocol.EffectiveStreams guarantees that ordering when
	// the report is assembled; this asserts the values actually arrived, which
	// they do not when an expression failed to evaluate.
	if len(report.Values) < len(opts.ABI) {
		return nil, fmt.Errorf("ReportCodecEVMABIEncodeUnpackedExpr not enough values for calculated streams; expected at least: %d, got: %d", len(opts.ABI), len(report.Values))
	}

	report.ValidAfterNanoseconds = ClampReportRange(r, report, opts.MaxReportRange)
	validAfter := protocol.ConvertTimestamp(report.ValidAfterNanoseconds, opts.TimeResolution)
	observationTimestamp := protocol.ConvertTimestamp(report.ObservationTimestampNanoseconds, opts.TimeResolution)
	expiresAt := observationTimestamp + protocol.ScaleSeconds(opts.ExpirationWindow, opts.TimeResolution)

	rf := BaseReportFields{
		FeedID:             opts.FeedID,
		ValidFromTimestamp: validAfter + 1,
		Timestamp:          observationTimestamp,
		NativeFee:          CalculateFee(nativePrice, opts.BaseUSDFee),
		LinkFee:            CalculateFee(linkPrice, opts.BaseUSDFee),
		ExpiresAt:          expiresAt,
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

// VerifyForAdmission implements protocol.AdmissionVerifier: it rejects
// statically invalid expressions before the definition can reach consensus. An
// expression that cannot be analyzed can never produce a value, so the channel
// would be installed and then never report.
//
// This is not part of Verify because definitions committed before the check
// existed may fail it, and rejecting those would stop every oracle from
// observing rather than just stopping that one channel from reporting.
//
// Like Verify it runs on an untrusted definition and is a pure function of it:
// this parses and analyzes only, with no stream values and no state.
//
// nil opts cache: the definition is given directly, so the opts are decoded from
// it rather than looked up.
func (r ReportCodecEVMABIEncodeUnpackedExpr) VerifyForAdmission(cd llotypes.ChannelDefinition) error {
	if err := calculated.ValidateChannelExpressions(nil, cd, 0); err != nil {
		return fmt.Errorf("invalid calculated stream expressions: %w", err)
	}
	return nil
}

func (r ReportCodecEVMABIEncodeUnpackedExpr) buildHeader(rf BaseReportFields, resolution protocol.TimeResolution) ([]byte, error) {
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
	if resolution == protocol.ResolutionSeconds {
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

// FeedID implements protocol.FeedIDer: these reports always carry a feed ID, and
// Verify has already rejected a zero one.
func (r ReportCodecEVMABIEncodeUnpackedExpr) FeedID(cd llotypes.ChannelDefinition) ([32]byte, bool, error) {
	opts := new(ReportFormatEVMABIEncodeOpts)
	if err := opts.Decode(cd.Opts); err != nil {
		return [32]byte{}, false, fmt.Errorf("invalid Opts, got: %q; %w", cd.Opts, err)
	}
	return opts.FeedID, true, nil
}
