package v3

import (
	"math/big"
	"testing"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ PAO = testPAO{}

type testPAO struct {
	Bid         *big.Int
	Benchmark   *big.Int
	Ask         *big.Int
	PricesValid bool
}

func (t testPAO) GetPrices() (prices Prices, valid bool) {
	return Prices{
		Benchmark: t.Benchmark,
		Bid:       t.Bid,
		Ask:       t.Ask,
	}, t.PricesValid
}

func (t testPAO) GetObserver() commontypes.OracleID       { return 0 }
func (t testPAO) GetTimestamp() uint32                    { return 0 }
func (t testPAO) GetBenchmarkPrice() (*big.Int, bool)     { return nil, false }
func (t testPAO) GetMaxFinalizedTimestamp() (int64, bool) { return 0, false }
func (t testPAO) GetLinkFee() (*big.Int, bool)            { return nil, false }
func (t testPAO) GetNativeFee() (*big.Int, bool)          { return nil, false }

func Test_GetConsensusPrices(t *testing.T) {
	tests := []struct {
		name string
		paos []PAO
		f    int
		want Prices
		err  string
	}{
		{
			name: "not enough valid observations",
			paos: []PAO{
				testPAO{
					PricesValid: false,
				},
				testPAO{
					PricesValid: false,
				},
				testPAO{
					Bid:         big.NewInt(20),
					Benchmark:   big.NewInt(21),
					Ask:         big.NewInt(23),
					PricesValid: true,
				},
			},
			f:    1,
			want: Prices{},
			err:  "fewer than f+1 observations have a valid price (got: 1/3)",
		},
		{
			name: "handles simple case",
			paos: []PAO{
				testPAO{
					Bid:         big.NewInt(1),
					Benchmark:   big.NewInt(2),
					Ask:         big.NewInt(3),
					PricesValid: true,
				},
				testPAO{
					Bid:         big.NewInt(9),
					Benchmark:   big.NewInt(9),
					Ask:         big.NewInt(9),
					PricesValid: true,
				},
				testPAO{
					Bid:         big.NewInt(20),
					Benchmark:   big.NewInt(21),
					Ask:         big.NewInt(23),
					PricesValid: true,
				},
			},
			f: 1,
			want: Prices{
				Bid:       big.NewInt(8),
				Benchmark: big.NewInt(9),
				Ask:       big.NewInt(9),
			},
			err: "",
		},
		{
			name: "handles simple inverted case",
			paos: []PAO{
				testPAO{
					Bid:         big.NewInt(1),
					Benchmark:   big.NewInt(2),
					Ask:         big.NewInt(3),
					PricesValid: true,
				},
				testPAO{
					Bid:         big.NewInt(10),
					Benchmark:   big.NewInt(9),
					Ask:         big.NewInt(8),
					PricesValid: true,
				},
				testPAO{
					Bid:         big.NewInt(20),
					Benchmark:   big.NewInt(21),
					Ask:         big.NewInt(23),
					PricesValid: true,
				},
			},
			f: 1,
			want: Prices{
				Bid:       big.NewInt(8),
				Benchmark: big.NewInt(9),
				Ask:       big.NewInt(9),
			},
			err: "",
		},
		{
			name: "handles complex inverted case (real world example)",
			paos: []PAO{
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
				testPAO{big.NewInt(1315243916), big.NewInt(1315661031), big.NewInt(1316078096), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
			},
			f:    5,
			want: Prices{Bid: big.NewInt(1315773517), Benchmark: big.NewInt(1316190800), Ask: big.NewInt(1316526999)},
			err:  "",
		},
		{
			name: "handles complex inverted case (f failures of various types)",
			paos: []PAO{
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315131262), big.NewInt(1314633333), big.NewInt(1315629191), true}, // inverted, bid > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask < benchmark
				testPAO{big.NewInt(2000000000), big.NewInt(1316190800), big.NewInt(1316527000), true}, // inverted, bid >>>> ask
				testPAO{big.NewInt(2000000000), big.NewInt(1315131262), big.NewInt(2001000000), true}, // inverted, bid >>>> benchmark
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1000000000), true}, // inverted, ask <<<< benchmark
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
				testPAO{big.NewInt(1315243916), big.NewInt(1315661031), big.NewInt(1316078096), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
			},
			f:    5,
			want: Prices{Bid: big.NewInt(1315854499), Benchmark: big.NewInt(1316190800), Ask: big.NewInt(1316526999)},
			err:  "",
		},
		{
			name: "handles complex inverted case (f failures skewed the same way)",
			paos: []PAO{
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1315661031), big.NewInt(1316078096), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
			},
			f:    5,
			want: Prices{Bid: big.NewInt(1315692469), Benchmark: big.NewInt(1316190800), Ask: big.NewInt(1316526999)},
			err:  "",
		},
		{
			name: "inverts output in complex inverted case with f+1 failures skewed the same way",
			paos: []PAO{
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1315854500), big.NewInt(1316190800), big.NewInt(1316527000), true},
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1315243916), big.NewInt(1316190800), big.NewInt(1316078096), true}, // inverted, ask > benchmark
				testPAO{big.NewInt(1314633333), big.NewInt(1315131262), big.NewInt(1315629191), true},
			},
			f:    5,
			want: Prices{Bid: big.NewInt(1315243915), Benchmark: big.NewInt(1316190800), Ask: big.NewInt(1316078096)}, // ask < bid but expected because f+1 failures is explicitly undefined
			err:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prices, err := GetConsensusPrices(tt.paos, tt.f)
			if tt.err != "" {
				require.Error(t, err)
				assert.Equal(t, tt.err, err.Error())
			}
			assert.Equal(t, tt.want, prices)
		})
	}
}
