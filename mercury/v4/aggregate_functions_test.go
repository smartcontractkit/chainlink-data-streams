package v4

import (
	"math/big"
	"testing"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testParsedAttributedObservation struct {
	Timestamp                  uint32
	BenchmarkPrice             *big.Int
	BenchmarkPriceValid        bool
	Bid                        *big.Int
	BidValid                   bool
	Ask                        *big.Int
	AskValid                   bool
	MaxFinalizedTimestamp      int64
	MaxFinalizedTimestampValid bool
	LinkFee                    *big.Int
	LinkFeeValid               bool
	NativeFee                  *big.Int
	NativeFeeValid             bool
	MarketStatus               uint32
	MarketStatusValid          bool
}

func (t testParsedAttributedObservation) GetObserver() commontypes.OracleID { return 0 }
func (t testParsedAttributedObservation) GetTimestamp() uint32              { return t.Timestamp }
func (t testParsedAttributedObservation) GetBenchmarkPrice() (*big.Int, bool) {
	return t.BenchmarkPrice, t.BenchmarkPriceValid
}
func (t testParsedAttributedObservation) GetBid() (*big.Int, bool) {
	return t.Bid, t.BidValid
}
func (t testParsedAttributedObservation) GetAsk() (*big.Int, bool) {
	return t.Ask, t.AskValid
}
func (t testParsedAttributedObservation) GetMaxFinalizedTimestamp() (int64, bool) {
	return t.MaxFinalizedTimestamp, t.MaxFinalizedTimestampValid
}
func (t testParsedAttributedObservation) GetLinkFee() (*big.Int, bool) {
	return t.LinkFee, t.LinkFeeValid
}
func (t testParsedAttributedObservation) GetNativeFee() (*big.Int, bool) {
	return t.NativeFee, t.NativeFeeValid
}
func (t testParsedAttributedObservation) GetMarketStatus() (uint32, bool) {
	return t.MarketStatus, t.MarketStatusValid
}

func convertTestPAOsToPAOs(testPAOs []testParsedAttributedObservation) []PAO {
	var paos []PAO
	for _, testPAO := range testPAOs {
		paos = append(paos, testPAO)
	}
	return paos
}

func newValidParsedAttributedObservations() []testParsedAttributedObservation {
	return []testParsedAttributedObservation{
		testParsedAttributedObservation{
			Timestamp: 1689648456,

			BenchmarkPrice:      big.NewInt(123),
			BenchmarkPriceValid: true,
			Bid:                 big.NewInt(120),
			BidValid:            true,
			Ask:                 big.NewInt(130),
			AskValid:            true,

			MaxFinalizedTimestamp:      1679448456,
			MaxFinalizedTimestampValid: true,

			LinkFee:        big.NewInt(1),
			LinkFeeValid:   true,
			NativeFee:      big.NewInt(1),
			NativeFeeValid: true,

			MarketStatus:      1,
			MarketStatusValid: true,
		},
		testParsedAttributedObservation{
			Timestamp: 1689648456,

			BenchmarkPrice:      big.NewInt(456),
			BenchmarkPriceValid: true,
			Bid:                 big.NewInt(450),
			BidValid:            true,
			Ask:                 big.NewInt(460),
			AskValid:            true,

			MaxFinalizedTimestamp:      1679448456,
			MaxFinalizedTimestampValid: true,

			LinkFee:        big.NewInt(2),
			LinkFeeValid:   true,
			NativeFee:      big.NewInt(2),
			NativeFeeValid: true,

			MarketStatus:      1,
			MarketStatusValid: true,
		},
		testParsedAttributedObservation{
			Timestamp: 1689648789,

			BenchmarkPrice:      big.NewInt(789),
			BenchmarkPriceValid: true,
			Bid:                 big.NewInt(780),
			BidValid:            true,
			Ask:                 big.NewInt(800),
			AskValid:            true,

			MaxFinalizedTimestamp:      1679448456,
			MaxFinalizedTimestampValid: true,

			LinkFee:        big.NewInt(3),
			LinkFeeValid:   true,
			NativeFee:      big.NewInt(3),
			NativeFeeValid: true,

			MarketStatus:      2,
			MarketStatusValid: true,
		},
		testParsedAttributedObservation{
			Timestamp: 1689648789,

			BenchmarkPrice:      big.NewInt(456),
			BenchmarkPriceValid: true,
			Bid:                 big.NewInt(450),
			BidValid:            true,
			Ask:                 big.NewInt(460),
			AskValid:            true,

			MaxFinalizedTimestamp:      1679513477,
			MaxFinalizedTimestampValid: true,

			LinkFee:        big.NewInt(4),
			LinkFeeValid:   true,
			NativeFee:      big.NewInt(4),
			NativeFeeValid: true,

			MarketStatus:      3,
			MarketStatusValid: true,
		},
	}
}

func NewValidParsedAttributedObservations(paos ...testParsedAttributedObservation) []testParsedAttributedObservation {
	if len(paos) == 0 {
		paos = newValidParsedAttributedObservations()
	}
	return []testParsedAttributedObservation{
		paos[0],
		paos[1],
		paos[2],
		paos[3],
	}
}

func NewInvalidParsedAttributedObservations() []testParsedAttributedObservation {
	return []testParsedAttributedObservation{
		testParsedAttributedObservation{
			Timestamp: 1,

			BenchmarkPrice:      big.NewInt(123),
			BenchmarkPriceValid: false,
			Bid:                 big.NewInt(120),
			BidValid:            false,
			Ask:                 big.NewInt(130),
			AskValid:            false,

			MaxFinalizedTimestamp:      1679648456,
			MaxFinalizedTimestampValid: false,

			LinkFee:        big.NewInt(1),
			LinkFeeValid:   false,
			NativeFee:      big.NewInt(1),
			NativeFeeValid: false,

			MarketStatus:      1,
			MarketStatusValid: false,
		},
		testParsedAttributedObservation{
			Timestamp: 2,

			BenchmarkPrice:      big.NewInt(456),
			BenchmarkPriceValid: false,
			Bid:                 big.NewInt(450),
			BidValid:            false,
			Ask:                 big.NewInt(460),
			AskValid:            false,

			MaxFinalizedTimestamp:      1679648456,
			MaxFinalizedTimestampValid: false,

			LinkFee:        big.NewInt(2),
			LinkFeeValid:   false,
			NativeFee:      big.NewInt(2),
			NativeFeeValid: false,

			MarketStatus:      1,
			MarketStatusValid: false,
		},
		testParsedAttributedObservation{
			Timestamp: 2,

			BenchmarkPrice:      big.NewInt(789),
			BenchmarkPriceValid: false,
			Bid:                 big.NewInt(780),
			BidValid:            false,
			Ask:                 big.NewInt(800),
			AskValid:            false,

			MaxFinalizedTimestamp:      1679648456,
			MaxFinalizedTimestampValid: false,

			LinkFee:        big.NewInt(3),
			LinkFeeValid:   false,
			NativeFee:      big.NewInt(3),
			NativeFeeValid: false,

			MarketStatus:      2,
			MarketStatusValid: false,
		},
		testParsedAttributedObservation{
			Timestamp: 3,

			BenchmarkPrice:      big.NewInt(456),
			BenchmarkPriceValid: true,
			Bid:                 big.NewInt(450),
			BidValid:            true,
			Ask:                 big.NewInt(460),
			AskValid:            true,

			MaxFinalizedTimestamp:      1679513477,
			MaxFinalizedTimestampValid: true,

			LinkFee:        big.NewInt(4),
			LinkFeeValid:   true,
			NativeFee:      big.NewInt(4),
			NativeFeeValid: true,

			MarketStatus:      3,
			MarketStatusValid: false,
		},
	}
}

func Test_AggregateFunctions(t *testing.T) {
	f := 1
	validPaos := NewValidParsedAttributedObservations()
	invalidPaos := NewInvalidParsedAttributedObservations()

	t.Run("GetConsensusMarketStatus", func(t *testing.T) {
		t.Run("gets consensus on market status when valid", func(t *testing.T) {
			marketStatus, err := GetConsensusMarketStatus(convertMarketStatus(convertTestPAOsToPAOs(validPaos)), f)
			require.NoError(t, err)
			assert.Equal(t, uint32(1), marketStatus)
		})
		t.Run("treats zero values as valid", func(t *testing.T) {
			paos := NewValidParsedAttributedObservations()
			for i := range paos {
				paos[i].MarketStatus = 0
			}
			marketStatus, err := GetConsensusMarketStatus(convertMarketStatus(convertTestPAOsToPAOs(paos)), f)
			require.NoError(t, err)
			assert.Equal(t, uint32(0), marketStatus)
		})
		t.Run("is stable during ties", func(t *testing.T) {
			paos := NewValidParsedAttributedObservations()
			for i := range paos {
				paos[i].MarketStatus = uint32(i % 2)
			}
			marketStatus, err := GetConsensusMarketStatus(convertMarketStatus(convertTestPAOsToPAOs(paos)), f)
			require.NoError(t, err)
			assert.Equal(t, uint32(0), marketStatus)
		})
		t.Run("fails when the mode is less than f+1", func(t *testing.T) {
			paos := NewValidParsedAttributedObservations()
			for i := range paos {
				paos[i].MarketStatus = uint32(i)
			}
			_, err := GetConsensusMarketStatus(convertMarketStatus(convertTestPAOsToPAOs(paos)), f)
			assert.EqualError(t, err, "market status has fewer than f+1 observations (status 0 got 1/4)")
		})
		t.Run("fails when all observations are invalid", func(t *testing.T) {
			_, err := GetConsensusMarketStatus(convertMarketStatus(convertTestPAOsToPAOs(invalidPaos)), f)
			assert.EqualError(t, err, "market status has fewer than f+1 observations (status 0 got 0/4)")
		})
	})
}
