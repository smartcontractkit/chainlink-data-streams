package protocol

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// countingReportCodec counts how often the per-definition checks are consulted,
// which is what the cache is there to avoid.
type countingReportCodec struct {
	verifies   atomic.Int64
	admissions atomic.Int64
	feedIDs    atomic.Int64

	verifyErr    error
	admissionErr error
	feedIDErr    error
}

func (c *countingReportCodec) Encode(Report, llotypes.ChannelDefinition, *OptsCache) ([]byte, error) {
	return nil, nil
}

func (c *countingReportCodec) Verify(llotypes.ChannelDefinition) error {
	c.verifies.Add(1)
	return c.verifyErr
}

func (c *countingReportCodec) VerifyForAdmission(llotypes.ChannelDefinition) error {
	c.admissions.Add(1)
	return c.admissionErr
}

func (c *countingReportCodec) FeedID(cd llotypes.ChannelDefinition) ([32]byte, bool, error) {
	c.feedIDs.Add(1)
	if c.feedIDErr != nil {
		return [32]byte{}, false, c.feedIDErr
	}
	var feedID [32]byte
	copy(feedID[:], cd.Opts)
	return feedID, true, nil
}

func (c *countingReportCodec) calls() (int64, int64, int64) {
	return c.verifies.Load(), c.admissions.Load(), c.feedIDs.Load()
}

func countingCodecs(codec *countingReportCodec) map[llotypes.ReportFormat]ReportCodec {
	return map[llotypes.ReportFormat]ReportCodec{llotypes.ReportFormat(0): codec}
}

func cacheTestDef(opts string) llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{
		Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
		Opts:    []byte(opts),
	}
}

func Test_ChannelAnalysisCache(t *testing.T) {
	t.Run("an unchanged definition is checked once across analyses", func(t *testing.T) {
		codec := &countingReportCodec{}
		defs := llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)}
		cache := NewChannelAnalysisCache()

		for i := 0; i < 5; i++ {
			require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, cache))
			require.NoError(t, VerifyChannelDefinitionsForAdmissionWithCache(countingCodecs(codec), defs, map[llotypes.ChannelID]struct{}{1: {}}, cache))
		}

		verifies, admissions, feedIDs := codec.calls()
		require.Equal(t, int64(1), verifies)
		require.Equal(t, int64(1), admissions)
		require.Equal(t, int64(1), feedIDs)
	})

	t.Run("a changed definition is checked again", func(t *testing.T) {
		codec := &countingReportCodec{}
		cache := NewChannelAnalysisCache()

		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)}, cache))
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":2}`)}, cache))

		verifies, _, _ := codec.calls()
		require.Equal(t, int64(2), verifies)
	})

	t.Run("a definition differing only in streams is checked again", func(t *testing.T) {
		codec := &countingReportCodec{}
		cache := NewChannelAnalysisCache()

		cd := cacheTestDef(`{"a":1}`)
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), llotypes.ChannelDefinitions{1: cd}, cache))
		cd.Streams = append(cd.Streams, llotypes.Stream{StreamID: 2, Aggregator: llotypes.AggregatorMedian})
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), llotypes.ChannelDefinitions{1: cd}, cache))

		verifies, _, _ := codec.calls()
		require.Equal(t, int64(2), verifies)
	})

	t.Run("the committed and the proposed value of a changing channel are both retained", func(t *testing.T) {
		codec := &countingReportCodec{}
		cache := NewChannelAnalysisCache()
		committed := llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)}
		proposed := llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":2}`)}

		// What a round does while a channel is being changed: verify the
		// committed set, then the one an observation advocates, repeatedly.
		for i := 0; i < 5; i++ {
			require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), committed, cache))
			require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), proposed, cache))
		}

		verifies, _, _ := codec.calls()
		require.Equal(t, int64(2), verifies)
	})

	t.Run("caching the entry does not alias the caller's definition", func(t *testing.T) {
		codec := &countingReportCodec{}
		cache := NewChannelAnalysisCache()

		cd := cacheTestDef(`{"a":1}`)
		defs := llotypes.ChannelDefinitions{1: cd}
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, cache))

		// Mutating the definition the cache was handed must not rewrite the
		// identity of the entry, or a different definition would read as a hit.
		cd.Opts[3] = '9'
		cd.Streams[0].StreamID = 7
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), llotypes.ChannelDefinitions{1: cd}, cache))

		verifies, _, _ := codec.calls()
		require.Equal(t, int64(2), verifies)
	})

	t.Run("Prune drops channels the committed set no longer holds", func(t *testing.T) {
		codec := &countingReportCodec{}
		cache := NewChannelAnalysisCache()
		defs := llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)}

		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, cache))
		cache.Prune(llotypes.ChannelDefinitions{})
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, cache))

		verifies, _, _ := codec.calls()
		require.Equal(t, int64(2), verifies)
	})

	t.Run("a nil cache memoizes nothing", func(t *testing.T) {
		codec := &countingReportCodec{}
		defs := llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)}

		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, nil))
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, nil))

		verifies, _, _ := codec.calls()
		require.Equal(t, int64(2), verifies)

		var nilCache *ChannelAnalysisCache
		nilCache.Prune(defs)
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, nilCache))
	})
}

// Test_ChannelAnalysisCache_SameFindings pins the cache to being invisible: the
// error a set produces must not depend on whether it, or an overlapping set,
// was analyzed before.
func Test_ChannelAnalysisCache_SameFindings(t *testing.T) {
	sharedFeedID := &countingReportCodec{}

	for _, tc := range []struct {
		name  string
		codec *countingReportCodec
		defs  llotypes.ChannelDefinitions
	}{
		{
			name:  "valid set",
			codec: &countingReportCodec{},
			defs:  llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`), 2: cacheTestDef(`{"a":2}`)},
		},
		{
			name:  "baseline failure",
			codec: &countingReportCodec{verifyErr: errors.New("bad opts")},
			defs:  llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`), 2: cacheTestDef(`{"a":2}`)},
		},
		{
			name:  "admission failure",
			codec: &countingReportCodec{admissionErr: errors.New("not admissible")},
			defs:  llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`), 2: cacheTestDef(`{"a":2}`)},
		},
		{
			name:  "feed ID failure",
			codec: &countingReportCodec{feedIDErr: errors.New("no feed ID")},
			defs:  llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)},
		},
		{
			name:  "duplicate feed ID across channels",
			codec: sharedFeedID,
			defs:  llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`), 2: cacheTestDef(`{"a":1}`)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codecs := countingCodecs(tc.codec)
			admitting := map[llotypes.ChannelID]struct{}{1: {}, 2: {}}

			want := VerifyChannelDefinitions(codecs, tc.defs)
			wantAdmission := VerifyChannelDefinitionsForAdmission(codecs, tc.defs, admitting)
			wantBad, wantSetErr := UnverifiableChannelIDs(codecs, tc.defs)

			cache := NewChannelAnalysisCache()
			// Warm the cache the way a round does, through a different entry
			// point than the one being compared.
			_, _ = UnverifiableChannelIDsWithCache(codecs, tc.defs, cache)

			got := VerifyChannelDefinitionsWithCache(codecs, tc.defs, cache)
			gotAdmission := VerifyChannelDefinitionsForAdmissionWithCache(codecs, tc.defs, admitting, cache)
			gotBad, gotSetErr := UnverifiableChannelIDsWithCache(codecs, tc.defs, cache)

			requireSameError(t, want, got)
			requireSameError(t, wantAdmission, gotAdmission)
			requireSameError(t, wantSetErr, gotSetErr)
			require.Equal(t, wantBad, gotBad)
		})
	}
}

func requireSameError(t *testing.T, want, got error) {
	t.Helper()
	if want == nil {
		require.NoError(t, got)
		return
	}
	require.EqualError(t, got, want.Error())
}

// Test_ChannelAnalysisCache_Concurrent exercises the access pattern of a round:
// Observation warming the cache while several ValidateObservation goroutines
// read it and each advocate a different update.
func Test_ChannelAnalysisCache_Concurrent(t *testing.T) {
	codec := &countingReportCodec{}
	codecs := countingCodecs(codec)
	cache := NewChannelAnalysisCache()

	committed := make(llotypes.ChannelDefinitions, 50)
	for i := llotypes.ChannelID(1); i <= 50; i++ {
		committed[i] = cacheTestDef(fmt.Sprintf(`{"a":%d}`, i))
	}
	want := VerifyChannelDefinitions(codecs, committed)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				requireSameError(t, want, VerifyChannelDefinitionsWithCache(codecs, committed, cache))

				updated := CloneChannelDefinitions(committed)
				channelID := llotypes.ChannelID(i%50 + 1)
				updated[channelID] = cacheTestDef(fmt.Sprintf(`{"a":%d,"b":%d}`, channelID, j))
				requireSameError(t, VerifyChannelDefinitions(codecs, updated), VerifyChannelDefinitionsWithCache(codecs, updated, cache))
			}
		}(i)
	}
	wg.Wait()
}
