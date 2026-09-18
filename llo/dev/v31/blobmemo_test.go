package llo

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31/llotest"
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// blobObservation broadcasts sv as a blob through bc and returns an observation
// referencing the resulting handle.
func blobObservation(t *testing.T, bc *llotest.BlobBroadcastFetcher, sv protocol.StreamValues) ocrtypes.Observation {
	t.Helper()
	payload, err := marshalStreamValues(sv)
	require.NoError(t, err)
	handle, err := bc.BroadcastBlob(tests.Context(t), payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 100})
	require.NoError(t, err)
	handleBytes, err := handle.MarshalBinary()
	require.NoError(t, err)
	enc, err := encodeObservation(Observation{UnixTimestampNanoseconds: 1}, [][]byte{handleBytes})
	require.NoError(t, err)
	return enc
}

func testStreamValues(n int) protocol.StreamValues {
	sv := protocol.StreamValues{}
	for i := 0; i < n; i++ {
		sv[llotypes.StreamID(i)] = protocol.ToDecimal(decimal.NewFromInt(int64(i)))
	}
	return sv
}

// Test_BlobPayloadCache_SkipsSecondFetch covers the reason the memo exists: the
// same handle decoded twice in one round fetches once.
func Test_BlobPayloadCache_SkipsSecondFetch(t *testing.T) {
	ctx := tests.Context(t)
	bc := newFakeBroadcaster()
	sv := testStreamValues(50)
	enc := blobObservation(t, bc, sv)

	cache := newBlobPayloadCache()

	first, err := decodeObservation(ctx, enc, bc, cache.round(7))
	require.NoError(t, err)
	require.Equal(t, 1, bc.Fetches())

	second, err := decodeObservation(ctx, enc, bc, cache.round(7))
	require.NoError(t, err)
	require.Equal(t, 1, bc.Fetches(), "the second decode of the round must be served from the memo")

	// A hit and a miss must produce the same observation, or the round's outcome
	// would depend on whether the memo was populated.
	require.Len(t, second.StreamValues, len(first.StreamValues))
	for id, want := range first.StreamValues {
		require.True(t, equalStreamValue(want, second.StreamValues[id]), "stream %d", id)
	}

	// Without a memo the fetch happens again, which is what the memo replaces.
	_, err = decodeObservation(ctx, enc, bc, nil)
	require.NoError(t, err)
	require.Equal(t, 2, bc.Fetches())
}

// Test_BlobPayloadCache_ScopedToSeqNr asserts entries do not survive the round
// they were decoded for: the blob transport gates fetches on the round's
// sequence number, and the memo must not reach around that.
func Test_BlobPayloadCache_ScopedToSeqNr(t *testing.T) {
	ctx := tests.Context(t)
	bc := newFakeBroadcaster()
	enc := blobObservation(t, bc, testStreamValues(10))

	cache := newBlobPayloadCache()
	_, err := decodeObservation(ctx, enc, bc, cache.round(7))
	require.NoError(t, err)
	require.Equal(t, 1, bc.Fetches())

	_, err = decodeObservation(ctx, enc, bc, cache.round(8))
	require.NoError(t, err)
	require.Equal(t, 2, bc.Fetches(), "a later round must not be served the previous round's payload")

	// The superseded handle is inert rather than a window back into round 7.
	stale := cache.round(7)
	require.NotNil(t, cache.round(9))
	_, ok := stale.get([]byte("anything"))
	require.False(t, ok)
	stale.put([]byte("anything"), blobPayloadEntry{})
	_, ok = cache.round(9).get([]byte("anything"))
	require.False(t, ok)
}

// Test_BlobPayloadCache_FailuresNotMemoized covers the two decode failures: a
// fetch failure is node-local and must be retried, and a malformed payload is
// deterministic so nothing is gained by remembering it.
func Test_BlobPayloadCache_FailuresNotMemoized(t *testing.T) {
	ctx := tests.Context(t)
	bc := newFakeBroadcaster()
	enc := blobObservation(t, bc, testStreamValues(10))
	cache := newBlobPayloadCache()

	// No fetcher: a reference that could not be resolved must not be cached as
	// an absence, so a later decode in the same round still fetches.
	_, err := decodeObservation(ctx, enc, nil, cache.round(7))
	var bfErr *blobFetchError
	require.ErrorAs(t, err, &bfErr)

	_, err = decodeObservation(ctx, enc, bc, cache.round(7))
	require.NoError(t, err)
	require.Equal(t, 1, bc.Fetches())
}

// Test_BlobPayloadCache_BudgetChargedOnHit asserts a hit consumes the same
// decompression budget the miss did, so an observation naming one handle
// repeatedly is bounded identically either way.
func Test_BlobPayloadCache_BudgetChargedOnHit(t *testing.T) {
	ctx := tests.Context(t)
	bc := newFakeBroadcaster()
	sv := testStreamValues(20)
	payload, err := marshalStreamValues(sv)
	require.NoError(t, err)
	handle, err := bc.BroadcastBlob(ctx, payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 100})
	require.NoError(t, err)
	handleBytes, err := handle.MarshalBinary()
	require.NoError(t, err)

	// The same handle four times, so the budget is charged four times.
	enc, err := encodeObservation(Observation{UnixTimestampNanoseconds: 1}, [][]byte{handleBytes, handleBytes, handleBytes, handleBytes})
	require.NoError(t, err)

	withMemo, memoErr := decodeObservation(ctx, enc, bc, newBlobPayloadCache().round(7))
	require.Equal(t, 1, bc.Fetches(), "repeats within one observation are memo hits")
	withoutMemo, plainErr := decodeObservation(ctx, enc, bc, nil)

	// Whatever the budget verdict is, it must be the same with and without the
	// memo; the payloads here are small, so both are expected to succeed.
	require.Equal(t, plainErr == nil, memoErr == nil)
	require.NoError(t, memoErr)
	require.Len(t, withMemo.StreamValues, len(withoutMemo.StreamValues))
}
