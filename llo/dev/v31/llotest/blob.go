// Package llotest provides test doubles for hosts that run the LLO OCR3.1
// reporting plugin outside of libocr (benchmarks, simulation harnesses,
// integration tests).
//
// The plugin disseminates stream values exclusively through blobs, so a host
// that passes a nil ocr3_1types.BlobBroadcastFetcher to
// PluginFactory.NewReportingPlugin gets a plugin whose observations never carry
// stream values (the blob pump detects the nil and stays inert rather than
// panicking). Such hosts should pass BlobBroadcastFetcher from this package
// instead.
package llotest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
)

// blobHandleVariantLightCertifiedBlob is the BlobHandle sum-type variant byte
// for a LightCertifiedBlob.
const blobHandleVariantLightCertifiedBlob = 0x01

// BlobBroadcastFetcher is an in-memory, content-addressed
// ocr3_1types.BlobBroadcastFetcher. Broadcast payloads are stored keyed by the
// handle they are addressed with, so any number of blobs may be in flight and
// each handle fetches back exactly its own payload.
//
// A real BlobHandle cannot be constructed outside libocr (the concrete type
// lives in an internal package), but one can be unmarshaled from a
// syntactically valid encoding, which is what this type does: a
// LightCertifiedBlob handle whose chunk-digests root is the SHA-256 of the
// payload. Handles are therefore distinct per payload and stable across
// re-broadcasts of identical payloads. There is no certification, no chunking
// and no expiry: expiration hints are recorded for assertions but never acted
// on.
//
// It is safe for concurrent use, which matters because the plugin broadcasts
// from its blob pump goroutine and fetches from the round goroutines. A single
// instance may be shared by every oracle in a simulated DON, which models a
// network where every broadcast blob is fetchable by every peer.
type BlobBroadcastFetcher struct {
	mu         sync.Mutex
	blobs      map[string][]byte
	hints      map[string]ocr3_1types.BlobExpirationHint
	hintLog    []ocr3_1types.BlobExpirationHint
	broadcasts int
	broadcastB int
	fetches    int
	err        error
}

var _ ocr3_1types.BlobBroadcastFetcher = &BlobBroadcastFetcher{}

// NewBlobBroadcastFetcher returns an empty in-memory broadcaster/fetcher.
func NewBlobBroadcastFetcher() *BlobBroadcastFetcher {
	return &BlobBroadcastFetcher{
		blobs: map[string][]byte{},
		hints: map[string]ocr3_1types.BlobExpirationHint{},
	}
}

// SetBroadcastError makes every subsequent BroadcastBlob fail with err (nil
// clears it), for exercising the plugin's broadcast-failure path: a round whose
// blob could not be broadcast carries no stream values.
func (b *BlobBroadcastFetcher) SetBroadcastError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// BroadcastBlob stores the payload and returns a handle addressing it.
func (b *BlobBroadcastFetcher) BroadcastBlob(_ context.Context, payload []byte, hint ocr3_1types.BlobExpirationHint) (ocr3_1types.BlobHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.broadcasts++
	if b.err != nil {
		return ocr3_1types.BlobHandle{}, b.err
	}

	handle, encoded, err := handleFor(payload)
	if err != nil {
		return ocr3_1types.BlobHandle{}, err
	}
	b.blobs[string(encoded)] = append([]byte(nil), payload...)
	b.hints[string(encoded)] = hint
	b.hintLog = append(b.hintLog, hint)
	b.broadcastB += len(payload)
	return handle, nil
}

// FetchBlob returns the payload the handle addresses.
func (b *BlobBroadcastFetcher) FetchBlob(_ context.Context, handle ocr3_1types.BlobHandle) ([]byte, error) {
	encoded, err := handle.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal blob handle: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.fetches++
	payload, ok := b.blobs[string(encoded)]
	if !ok {
		return nil, errors.New("llotest: no blob was broadcast for this handle")
	}
	return append([]byte(nil), payload...), nil
}

// Broadcasts reports how many times BroadcastBlob was called, including failed
// calls.
func (b *BlobBroadcastFetcher) Broadcasts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.broadcasts
}

// BroadcastBytes reports the total payload bytes of successful broadcasts. It
// lets a benchmark account for the bytes that left the observation when stream
// values moved into a blob.
func (b *BlobBroadcastFetcher) BroadcastBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.broadcastB
}

// Fetches reports how many times FetchBlob was called.
func (b *BlobBroadcastFetcher) Fetches() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fetches
}

// Blobs reports how many distinct payloads are stored.
func (b *BlobBroadcastFetcher) Blobs() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.blobs)
}

// Hints returns the expiration hints of all successful broadcasts, in call
// order. Unlike ExpirationHint it distinguishes repeat broadcasts of an
// identical payload, which are content-addressed to the same handle.
func (b *BlobBroadcastFetcher) Hints() []ocr3_1types.BlobExpirationHint {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ocr3_1types.BlobExpirationHint(nil), b.hintLog...)
}

// ExpirationHint returns the hint the payload was most recently broadcast with,
// or nil if the payload was never broadcast.
func (b *BlobBroadcastFetcher) ExpirationHint(payload []byte) ocr3_1types.BlobExpirationHint {
	_, encoded, err := handleFor(payload)
	if err != nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hints[string(encoded)]
}

// NewBlobHandle returns a syntactically valid BlobHandle addressing payload,
// for tests that need a handle without a broadcaster (e.g. to build an
// observation referencing an unfetchable blob).
func NewBlobHandle(payload []byte) (ocr3_1types.BlobHandle, error) {
	handle, _, err := handleFor(payload)
	return handle, err
}

// handleFor builds the handle addressing payload, plus its wire encoding.
//
// The encoding is the minimum a LightCertifiedBlob accepts: the sum-type
// variant byte, then a protobuf carrying only chunk_digests_root (field 1, a
// 32-byte value) set to the SHA-256 of the payload.
func handleFor(payload []byte) (ocr3_1types.BlobHandle, []byte, error) {
	digest := sha256.Sum256(payload)
	encoded := make([]byte, 0, 3+sha256.Size)
	encoded = append(encoded, blobHandleVariantLightCertifiedBlob)
	encoded = append(encoded, 0x0A, sha256.Size) // proto field 1, wire type 2 (bytes), length 32
	encoded = append(encoded, digest[:]...)

	var handle ocr3_1types.BlobHandle
	if err := handle.UnmarshalBinary(encoded); err != nil {
		return ocr3_1types.BlobHandle{}, nil, fmt.Errorf("llotest: build blob handle: %w", err)
	}
	return handle, encoded, nil
}
