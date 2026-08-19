package protocol

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type genTestOpts struct {
	V int `json:"v"`
}

func genTestDefs(v int) llotypes.ChannelDefinitions {
	return llotypes.ChannelDefinitions{
		1: {
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
			Opts:         llotypes.ChannelOpts(fmt.Sprintf(`{"v":%d}`, v)),
		},
	}
}

func Test_ChannelCache_MemoizesBySeqNr(t *testing.T) {
	c := NewChannelCache()

	builds := 0
	build := func(v int) func() (llotypes.ChannelDefinitions, error) {
		return func() (llotypes.ChannelDefinitions, error) {
			builds++
			return genTestDefs(v), nil
		}
	}

	first, err := c.Load(7, build(1))
	require.NoError(t, err)
	require.Equal(t, 1, builds)

	// Same seqNr resolves to the same generation without rebuilding: the record
	// is a pure function of its sequence number.
	again, err := c.Load(7, build(2))
	require.NoError(t, err)
	require.Same(t, first, again)
	require.Equal(t, 1, builds)

	other, err := c.Load(8, build(2))
	require.NoError(t, err)
	require.NotSame(t, first, other)
	require.Equal(t, 2, builds)

	// Decoded opts belong to their own generation.
	o1, err := GetOpts[genTestOpts](first.Opts(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, o1.V)
	o2, err := GetOpts[genTestOpts](other.Opts(), 1)
	require.NoError(t, err)
	require.Equal(t, 2, o2.V)
}

func Test_ChannelCache_NilReceiverBuildsEveryTime(t *testing.T) {
	var c *ChannelCache

	gen, err := c.Load(1, func() (llotypes.ChannelDefinitions, error) { return genTestDefs(3), nil })
	require.NoError(t, err)
	require.Equal(t, uint64(1), gen.SeqNr())

	o, err := GetOpts[genTestOpts](gen.Opts(), 1)
	require.NoError(t, err)
	require.Equal(t, 3, o.V)
}

func Test_ChannelCache_LoadPropagatesBuildError(t *testing.T) {
	c := NewChannelCache()
	_, err := c.Load(1, func() (llotypes.ChannelDefinitions, error) {
		return nil, fmt.Errorf("boom")
	})
	require.ErrorContains(t, err, "boom")

	// The failure must not be memoized.
	gen, err := c.Load(1, func() (llotypes.ChannelDefinitions, error) { return genTestDefs(1), nil })
	require.NoError(t, err)
	require.NotNil(t, gen)
}

func Test_ChannelCache_EvictionDoesNotInvalidateHeldGeneration(t *testing.T) {
	c := NewChannelCache()

	held, err := c.Load(1, func() (llotypes.ChannelDefinitions, error) { return genTestDefs(1), nil })
	require.NoError(t, err)

	for seqNr := uint64(2); seqNr <= uint64(channelGenerationsRetained+3); seqNr++ {
		v := int(seqNr)
		_, err := c.Load(seqNr, func() (llotypes.ChannelDefinitions, error) { return genTestDefs(v), nil })
		require.NoError(t, err)
	}

	require.Len(t, c.gens, channelGenerationsRetained, "cache must stay bounded")
	_, ok := c.get(1)
	require.False(t, ok, "the oldest generation must have been evicted from the index")

	// A round still holding the evicted generation is unaffected: eviction only
	// drops the memo entry, never the snapshot itself.
	o, err := GetOpts[genTestOpts](held.Opts(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, o.V)
	require.Equal(t, uint64(1), held.SeqNr())
}

func Test_ChannelGeneration_OptsAreSealed(t *testing.T) {
	c := NewChannelCache()
	gen, err := c.Load(1, func() (llotypes.ChannelDefinitions, error) { return genTestDefs(1), nil })
	require.NoError(t, err)

	// Every mutator is a no-op: a generation's opts can never be repointed at
	// another record, whatever a concurrently running round does.
	gen.Opts().Set(1, llotypes.ChannelOpts(`{"v":99}`))
	gen.Opts().Remove(1)
	gen.Opts().ResetTo(llotypes.ChannelDefinitions{})

	o, err := GetOpts[genTestOpts](gen.Opts(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, o.V)
	require.Equal(t, 1, gen.Opts().Len())
}

func Test_ChannelGeneration_DoesNotAliasItsSource(t *testing.T) {
	c := NewChannelCache()

	source := genTestDefs(1)
	gen, err := c.Load(1, func() (llotypes.ChannelDefinitions, error) { return source, nil })
	require.NoError(t, err)

	// Mutating the source after the build must not reach the snapshot.
	cd := source[1]
	cd.Streams = append(cd.Streams, llotypes.Stream{StreamID: 999, Aggregator: llotypes.AggregatorCalculated})
	source[1] = cd
	source[2] = llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON}
	copy(source[1].Opts, `{"v":9}`)

	require.Len(t, gen.Definitions(), 1)
	require.Len(t, gen.Definitions()[1].Streams, 1)

	o, err := GetOpts[genTestOpts](gen.Opts(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, o.V)
}

func Test_CloneChannelDefinitions_IsDeep(t *testing.T) {
	in := genTestDefs(1)
	out := CloneChannelDefinitions(in)

	cd := out[1]
	// A shallow clone would let this append land in the original's backing array
	// whenever it has spare capacity.
	cd.Streams = append(cd.Streams, llotypes.Stream{StreamID: 999, Aggregator: llotypes.AggregatorCalculated})
	out[1] = cd
	copy(out[1].Opts, `{"v":9}`)

	require.Len(t, in[1].Streams, 1)
	require.Equal(t, llotypes.StreamID(100), in[1].Streams[0].StreamID)
	require.JSONEq(t, `{"v":1}`, string(in[1].Opts))
}

func Test_ChannelCache_ConcurrentLoadYieldsOneGeneration(t *testing.T) {
	c := NewChannelCache()

	const goroutines = 16
	gens := make([]*ChannelGeneration, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gen, err := c.Load(1, func() (llotypes.ChannelDefinitions, error) { return genTestDefs(1), nil })
			require.NoError(t, err)
			o, err := GetOpts[genTestOpts](gen.Opts(), 1)
			require.NoError(t, err)
			require.Equal(t, 1, o.V)
			gens[i] = gen
		}(i)
	}
	wg.Wait()

	for i := 1; i < goroutines; i++ {
		require.Same(t, gens[0], gens[i], "a seqNr must resolve to a single generation")
	}
}
