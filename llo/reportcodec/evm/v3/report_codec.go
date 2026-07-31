package v3

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
)

type ReportFields struct {
	ValidFromTimestamp uint32
	Timestamp          uint32
	NativeFee          *big.Int
	LinkFee            *big.Int
	ExpiresAt          uint32
	BenchmarkPrice     *big.Int
	Bid                *big.Int
	Ask                *big.Int
}

var zero = big.NewInt(0)

type ReportCodec struct {
	logger logger.Logger
	feedID common.Hash
}

func NewReportCodec(feedID [32]byte, lggr logger.Logger) *ReportCodec {
	return &ReportCodec{lggr, feedID}
}

func (r *ReportCodec) BuildReport(rf ReportFields) (ocrtypes.Report, error) {
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
	if err != nil {
		return nil, fmt.Errorf("failed to pack report blob: %w", err)
	}
	return ocrtypes.Report(reportBytes), nil
}

func (r *ReportCodec) Decode(report ocrtypes.Report) (*Report, error) {
	return Decode(report)
}
