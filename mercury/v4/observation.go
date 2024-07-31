package v4

import (
	"math/big"

	"github.com/smartcontractkit/libocr/commontypes"

	"github.com/smartcontractkit/chainlink-data-streams/mercury"
)

type PAO interface {
	mercury.PAO
	GetBid() (*big.Int, bool)
	GetAsk() (*big.Int, bool)
	GetMaxFinalizedTimestamp() (int64, bool)
	GetLinkFee() (*big.Int, bool)
	GetNativeFee() (*big.Int, bool)
	GetMarketStatus() (uint32, bool)
}

var _ PAO = parsedAttributedObservation{}

type parsedAttributedObservation struct {
	Timestamp uint32
	Observer  commontypes.OracleID

	BenchmarkPrice *big.Int
	Bid            *big.Int
	Ask            *big.Int
	PricesValid    bool

	MaxFinalizedTimestamp      int64
	MaxFinalizedTimestampValid bool

	LinkFee      *big.Int
	LinkFeeValid bool

	NativeFee      *big.Int
	NativeFeeValid bool

	MarketStatus      uint32
	MarketStatusValid bool
}

func NewParsedAttributedObservation(ts uint32, observer commontypes.OracleID,
	bp *big.Int, bid *big.Int, ask *big.Int, pricesValid bool, mfts int64,
	mftsValid bool, linkFee *big.Int, linkFeeValid bool, nativeFee *big.Int, nativeFeeValid bool,
	marketStatus uint32, marketStatusValid bool) PAO {
	return parsedAttributedObservation{
		Timestamp: ts,
		Observer:  observer,

		BenchmarkPrice: bp,
		Bid:            bid,
		Ask:            ask,
		PricesValid:    pricesValid,

		MaxFinalizedTimestamp:      mfts,
		MaxFinalizedTimestampValid: mftsValid,

		LinkFee:      linkFee,
		LinkFeeValid: linkFeeValid,

		NativeFee:      nativeFee,
		NativeFeeValid: nativeFeeValid,

		MarketStatus:      marketStatus,
		MarketStatusValid: marketStatusValid,
	}
}

func (pao parsedAttributedObservation) GetTimestamp() uint32 {
	return pao.Timestamp
}

func (pao parsedAttributedObservation) GetObserver() commontypes.OracleID {
	return pao.Observer
}

func (pao parsedAttributedObservation) GetBenchmarkPrice() (*big.Int, bool) {
	return pao.BenchmarkPrice, pao.PricesValid
}

func (pao parsedAttributedObservation) GetBid() (*big.Int, bool) {
	return pao.Bid, pao.PricesValid
}

func (pao parsedAttributedObservation) GetAsk() (*big.Int, bool) {
	return pao.Ask, pao.PricesValid
}

func (pao parsedAttributedObservation) GetMaxFinalizedTimestamp() (int64, bool) {
	if pao.MaxFinalizedTimestamp < -1 {
		// values below -1 are not valid
		return 0, false
	}
	return pao.MaxFinalizedTimestamp, pao.MaxFinalizedTimestampValid
}

func (pao parsedAttributedObservation) GetLinkFee() (*big.Int, bool) {
	return pao.LinkFee, pao.LinkFeeValid
}

func (pao parsedAttributedObservation) GetNativeFee() (*big.Int, bool) {
	return pao.NativeFee, pao.NativeFeeValid
}

func (pao parsedAttributedObservation) GetMarketStatus() (uint32, bool) {
	return pao.MarketStatus, pao.MarketStatusValid
}
