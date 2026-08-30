package llotest

import (
	"context"
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

func Test_BlobBroadcastFetcher_WaitForBroadcast(t *testing.T) {
	ctx := tests.Context(t)
	b := NewBlobBroadcastFetcher()

	// Already satisfied: returns without blocking.
	_, err := b.BroadcastBlob(ctx, []byte("first"), ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5})
	require.NoError(t, err)
	require.NoError(t, b.WaitForBroadcast(ctx, 0))

	// Blocks until a concurrent broadcast lands.
	done := make(chan error, 1)
	go func() { done <- b.WaitForBroadcast(ctx, 1) }()
	_, err = b.BroadcastBlob(ctx, []byte("second"), ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 6})
	require.NoError(t, err)
	require.NoError(t, <-done)

	// Failed broadcasts also count, so a waiter never hangs on a broken
	// broadcaster.
	b.SetBroadcastError(errors.New("broadcast unavailable"))
	go func() {
		_, bErr := b.BroadcastBlob(ctx, []byte("third"), ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 7})
		done <- bErr
	}()
	require.NoError(t, b.WaitForBroadcast(ctx, 2))
	require.Error(t, <-done)

	// Context expiry is reported, not hidden.
	expired, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, b.WaitForBroadcast(expired, 100), context.Canceled)
}
