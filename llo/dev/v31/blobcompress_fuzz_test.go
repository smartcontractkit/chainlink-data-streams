package llo

import (
	"testing"
)

// FuzzDecodeBlobPayload feeds arbitrary bytes through the blob payload decoder.
// Blob payloads are attacker-controlled, so the contract is that any input
// either returns bytes within the caller's budget or errors. Never a panic,
// and never more bytes than the budget allows (a zstd bomb).
func FuzzDecodeBlobPayload(f *testing.F) {
	raw := []byte("stream values would go here")
	framed, err := encodeBlobPayload(raw)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(framed)
	f.Add(append([]byte{blobCodecRaw}, raw...))
	f.Add([]byte{blobCodecZstd})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, payload []byte) {
		for _, budget := range []int{0, 1, 1 << 10, maxDecompressedBlobPayloadBytes} {
			out, err := decodeBlobPayload(payload, budget)
			if err != nil {
				continue
			}
			if len(out) > budget {
				t.Fatalf("decoded %d bytes against a budget of %d", len(out), budget)
			}
		}
	})
}
