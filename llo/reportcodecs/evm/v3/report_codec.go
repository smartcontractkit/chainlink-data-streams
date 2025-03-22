package v3

import (
	"errors"
	"fmt"
	"math/big"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	v3 "github.com/smartcontractkit/chainlink-common/pkg/types/mercury/v3"
)

var zero = big.NewInt(0)

type ReportCodec struct {
	logger logger.Logger
	feedID FeedID
}

func NewReportCodec(feedID [32]byte, lggr logger.Logger) *ReportCodec {
	return &ReportCodec{lggr, feedID}
}

func (r *ReportCodec) BuildReport(rf v3.ReportFields) (ocrtypes.Report, error) {
	var merr error
	if rf.BenchmarkPrice == nil {
		merr = errors.Join(merr, errors.New("benchmarkPrice may not be nil"))
	}
	if rf.Bid == nil {
		merr = errors.Join(merr, errors.New("bid may not be nil"))
	}
	if rf.Ask == nil {
		merr = errors.Join(merr, errors.New("ask may not be nil"))
	}
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
	reportBytes, err := Schema.Pack(r.feedID, rf.ValidFromTimestamp, rf.Timestamp, rf.NativeFee, rf.LinkFee, rf.ExpiresAt, rf.BenchmarkPrice, rf.Bid, rf.Ask)
	return ocrtypes.Report(reportBytes), fmt.Errorf("failed to pack report blob: %w", err)
}

func (r *ReportCodec) Decode(report ocrtypes.Report) (*Report, error) {
	return Decode(report)
}
