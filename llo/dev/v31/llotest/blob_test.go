package llotest

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"

	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
)

func Test_BlobBroadcastFetcher_RoundTrip(t *testing.T) {
	ctx := tests.Context(t)
	b := NewBlobBroadcastFetcher()

	h1, err := b.BroadcastBlob(ctx, []byte("first"), ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5})
	require.NoError(t, err)
	h2, err := b.BroadcastBlob(ctx, []byte("second"), ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 6})
	require.NoError(t, err)

	// Handles are content-addressed, so concurrently-live blobs stay distinct.
	e1, err := h1.MarshalBinary()
	require.NoError(t, err)
	e2, err := h2.MarshalBinary()
	require.NoError(t, err)
	require.NotEqual(t, e1, e2)

	got1, err := b.FetchBlob(ctx, h1)
	require.NoError(t, err)
	require.Equal(t, []byte("first"), got1)
	got2, err := b.FetchBlob(ctx, h2)
	require.NoError(t, err)
	require.Equal(t, []byte("second"), got2)

	require.Equal(t, 2, b.Broadcasts())
	require.Equal(t, 2, b.Fetches())
	require.Equal(t, 2, b.Blobs())
	require.Equal(t, len("first")+len("second"), b.BroadcastBytes())
	require.Equal(t, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 6}, b.ExpirationHint([]byte("second")))
	require.Equal(t, []ocr3_1types.BlobExpirationHint{
		ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5},
		ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 6},
	}, b.Hints())
}

func Test_BlobBroadcastFetcher_UnknownHandle(t *testing.T) {
	ctx := tests.Context(t)
	handle, err := NewBlobHandle([]byte("never broadcast"))
	require.NoError(t, err)

	_, err = NewBlobBroadcastFetcher().FetchBlob(ctx, handle)
	require.ErrorContains(t, err, "no blob was broadcast")
}

func Test_BlobBroadcastFetcher_BroadcastError(t *testing.T) {
	ctx := tests.Context(t)
	b := NewBlobBroadcastFetcher()
	b.SetBroadcastError(errors.New("broadcast unavailable"))

	_, err := b.BroadcastBlob(ctx, []byte("payload"), ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5})
	require.ErrorContains(t, err, "broadcast unavailable")
	require.Equal(t, 1, b.Broadcasts())
	require.Zero(t, b.Blobs())

	b.SetBroadcastError(nil)
	_, err = b.BroadcastBlob(ctx, []byte("payload"), ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5})
	require.NoError(t, err)
	require.Equal(t, 1, b.Blobs())
}
