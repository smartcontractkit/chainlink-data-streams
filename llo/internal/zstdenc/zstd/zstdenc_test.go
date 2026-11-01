package zstd_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"testing"

	upstream "github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-data-streams/llo/internal/zstdenc/zstd"
)

// upstreamVersion is the klauspost/compress release this package was copied
// from. The differential tests only hold against that release.
const upstreamVersion = "v1.20.3"

// corpusDigest freezes the output for the whole corpus. It does not depend on
// the upstream module, so it guards the copy even after the decoder moves on.
const corpusDigest = "2b0fbc61999a0c977cf14f8ecd886a6a8fd68d7fc728b559760ac9cb2e8b1f3c"

func newEncoder(t testing.TB) *zstd.Encoder {
	t.Helper()
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(true),
		zstd.WithZeroFrames(false),
		zstd.WithSingleSegment(true),
	)
	require.NoError(t, err)
	return enc
}

func newUpstreamEncoder(t testing.TB) *upstream.Encoder {
	t.Helper()
	enc, err := upstream.NewWriter(nil,
		upstream.WithEncoderLevel(upstream.SpeedDefault),
		upstream.WithEncoderCRC(true),
		upstream.WithZeroFrames(false),
		upstream.WithSingleSegment(true),
	)
	require.NoError(t, err)
	return enc
}

func requireUpstreamVersion(t testing.TB) {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok)
	for _, dep := range info.Deps {
		if dep.Path == "github.com/klauspost/compress" {
			if dep.Version != upstreamVersion {
				t.Skipf("upstream is %s, the copy is of %s", dep.Version, upstreamVersion)
			}
			return
		}
	}
	t.Skip("upstream module not in build info")
}

type input struct {
	name string
	data []byte
}

// corpus covers the encoder paths: empty, raw and RLE blocks, inputs on both
// sides of the 256 byte and 1 KiB frame header thresholds, a single block,
// several 128 KiB blocks, and literal heavy and match heavy content.
func corpus() []input {
	rng := rand.New(rand.NewPCG(1, 2))
	var out []input
	for _, n := range []int{0, 1, 40, 255, 256, 1000, 1024, 1025, 4096, 128 << 10, 128<<10 + 1, 300_000, 1 << 20, 3 << 20} {
		random := make([]byte, n)
		for i := range random {
			random[i] = byte(rng.Uint32())
		}
		text := make([]byte, n)
		for i := range text {
			text[i] = byte('a' + rng.IntN(6))
			if rng.IntN(4) == 0 && i > 64 {
				text[i] = text[i-1-rng.IntN(64)]
			}
		}
		out = append(out,
			input{fmt.Sprintf("zeros/%d", n), make([]byte, n)},
			input{fmt.Sprintf("random/%d", n), random},
			input{fmt.Sprintf("text/%d", n), text},
		)
	}
	return out
}

func TestEncodeAll_MatchesUpstream(t *testing.T) {
	requireUpstreamVersion(t)
	ours, theirs := newEncoder(t), newUpstreamEncoder(t)
	for _, in := range corpus() {
		require.Equal(t, theirs.EncodeAll(in.data, nil), ours.EncodeAll(in.data, nil), in.name)
	}
}

func TestEncodeAll_RoundTrips(t *testing.T) {
	enc := newEncoder(t)
	dec, err := upstream.NewReader(nil)
	require.NoError(t, err)
	defer dec.Close()
	for _, in := range corpus() {
		if len(in.data) == 0 {
			continue // WithZeroFrames(false) writes no frame for empty input.
		}
		got, err := dec.DecodeAll(enc.EncodeAll(in.data, nil), nil)
		require.NoError(t, err, in.name)
		require.Equal(t, in.data, got, in.name)
	}
}

func TestEncodeAll_CorpusDigest(t *testing.T) {
	enc := newEncoder(t)
	h := sha256.New()
	for _, in := range corpus() {
		b := enc.EncodeAll(in.data, nil)
		fmt.Fprintf(h, "%s|%d|", in.name, len(b))
		h.Write(b)
	}
	require.Equal(t, corpusDigest, hex.EncodeToString(h.Sum(nil)))
}

func TestEncodeAll_DeterministicAcrossGoroutines(t *testing.T) {
	// Encoders are pooled and reused, so an encode must not depend on what
	// the pooled encoder compressed before.
	enc := newEncoder(t)
	inputs := corpus()
	want := make([][]byte, len(inputs))
	for i, in := range inputs {
		want[i] = enc.EncodeAll(in.data, nil)
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(g), 7))
			for _, i := range rng.Perm(len(inputs)) {
				if got := enc.EncodeAll(inputs[i].data, nil); string(got) != string(want[i]) {
					t.Errorf("%s: output differs", inputs[i].name)
				}
			}
		})
	}
	wg.Wait()
}

func FuzzEncodeAll_MatchesUpstream(f *testing.F) {
	requireUpstreamVersion(f)
	for _, in := range corpus() {
		if len(in.data) <= 4096 {
			f.Add(in.data)
		}
	}
	ours, theirs := newEncoder(f), newUpstreamEncoder(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		require.Equal(t, theirs.EncodeAll(data, nil), ours.EncodeAll(data, nil))
	})
}
