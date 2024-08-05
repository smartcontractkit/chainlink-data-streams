package mercury

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/smartcontractkit/chainlink-common/pkg/types/mercury"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
)

func FuzzDecodeOnchainConfig(f *testing.F) {
	valid, err := StandardOnchainConfigCodec{}.Encode(tests.Context(f), mercury.OnchainConfig{Min: big.NewInt(1), Max: big.NewInt(1000)})
	if err != nil {
		f.Fatalf("failed to construct valid OnchainConfig: %s", err)
	}

	f.Add([]byte{})
	f.Add(valid)
	f.Fuzz(func(t *testing.T, encoded []byte) {
		ctx := tests.Context(t)
		decoded, err := StandardOnchainConfigCodec{}.Decode(ctx, encoded)
		if err != nil {
			return
		}

		encoded2, err := StandardOnchainConfigCodec{}.Encode(ctx, decoded)
		if err != nil {
			t.Fatalf("failed to re-encode decoded input: %s", err)
		}

		if !bytes.Equal(encoded, encoded2) {
			t.Fatalf("re-encoding of decoded input %x did not match original input %x", encoded2, encoded)
		}
	})
}
