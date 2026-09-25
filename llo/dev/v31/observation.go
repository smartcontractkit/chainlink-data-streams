package llo

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	"google.golang.org/protobuf/proto"
)

// Observation is the decoded per-round observation. It mirrors the v30
// Observation, but is disseminated using the v31 wire framing (which supports
// offloading the bulk stream-value payload to a blob).
type Observation struct {
	AttestedPredecessorRetirement []byte
	ShouldRetire                  bool
	UnixTimestampNanoseconds      uint64
	RemoveChannelIDs              map[llotypes.ChannelID]struct{}
	UpdateChannelDefinitions      llotypes.ChannelDefinitions
	StreamValues                  protocol.StreamValues
	// SupportedReportFormats are the report formats this oracle has a report
	// codec for. Encoding is node-local state that the state transition cannot
	// read without forking; advertising it here turns it into a replicated fact
	// that reportability can gate on (see isReportable).
	SupportedReportFormats map[llotypes.ReportFormat]struct{}
	// PredecessorSigners and PredecessorF are the predecessor instance's signer
	// set and f, read from the node-local retirement report cache. A staging
	// instance carries them alongside an attested retirement report, so the
	// state transition can agree on the set by vote and verify the report
	// against it; see resolvePredecessorRetirement.
	//
	// Signer order is significant: a signature names its signer by index.
	PredecessorSigners [][]byte
	PredecessorF       uint8
}

// observationWireVersion is the leading byte of the v31 observation framing.
const observationWireVersion byte = 1

// maxObservationBlobHandles bounds the number of blob handles a single
// observation may reference. The encoder emits at most one, so this leaves
// headroom for chunking a future oversized payload while keeping the count
// tight: every handle a peer names is decompression work each other oracle
// performs, and all N-1 attributed observations decode in parallel inside
// StateTransition. Raising this multiplies the memory a single byzantine peer
// can force per round; see also maxObservationDecompressedBytes.
const maxObservationBlobHandles = 4

// maxObservationDecompressedBytes bounds the TOTAL decompressed blob bytes a
// single observation may cause, across all of its handles. Without it, the
// per-blob cap alone would let one peer force
// maxObservationBlobHandles * maxDecompressedBlobPayloadBytes of decompression.
const maxObservationDecompressedBytes = maxDecompressedBlobPayloadBytes

// encodeObservation serializes an Observation into the v31 wire frame. Stream
// values are never carried inline: they are broadcast as a blob by the blob pump
// and referenced here by the marshaled handle(s) it produced. An observation
// with no handles simply carries no stream values.
func encodeObservation(obs Observation, handles [][]byte) (ocrtypes.Observation, error) {
	main := &protocol.LLOObservationProto{
		AttestedPredecessorRetirement: obs.AttestedPredecessorRetirement,
		ShouldRetire:                  obs.ShouldRetire,
		UnixTimestampNanoseconds:      obs.UnixTimestampNanoseconds,
		PredecessorSigners:            obs.PredecessorSigners,
		PredecessorF:                  uint32(obs.PredecessorF),
	}
	for id := range obs.RemoveChannelIDs {
		main.RemoveChannelIDs = append(main.RemoveChannelIDs, id)
	}
	// Map iteration order, so sort: Deterministic marshaling canonicalizes
	// proto map fields, not a repeated field built from a Go map.
	sortChannelIDs(main.RemoveChannelIDs)
	if len(obs.UpdateChannelDefinitions) > 0 {
		main.UpdateChannelDefinitions = make(map[uint32]*protocol.LLOChannelDefinitionProto, len(obs.UpdateChannelDefinitions))
		for id, cd := range obs.UpdateChannelDefinitions {
			main.UpdateChannelDefinitions[id] = protocol.ChannelDefinitionToProto(cd)
		}
	}

	// Map iteration order, so sort: the wire form is a repeated field, which
	// deterministic marshaling does not canonicalize.
	main.SupportedReportFormats = sortedFormatsToWire(obs.SupportedReportFormats)

	// Deterministic even though nothing compares observation bytes today
	// A future path which does compare or hash the value cannot be
	// silently wrong.
	mainBytes, err := deterministicMarshal.Marshal(main)
	if err != nil {
		return nil, fmt.Errorf("marshal observation: %w", err)
	}
	return frameObservation(handles, mainBytes), nil
}

// frameObservation builds the wire frame: version byte, uvarint handle count,
// each handle length-prefixed, then the main proto bytes.
func frameObservation(handles [][]byte, mainBytes []byte) []byte {
	buf := make([]byte, 0, 1+binary.MaxVarintLen64+len(mainBytes))
	buf = append(buf, observationWireVersion)
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(handles)))
	buf = append(buf, tmp[:n]...)
	for _, h := range handles {
		n = binary.PutUvarint(tmp[:], uint64(len(h)))
		buf = append(buf, tmp[:n]...)
		buf = append(buf, h...)
	}
	return append(buf, mainBytes...)
}

// blobFetchError wraps a failure to fetch (or access) a blob referenced by an
// observation. It exists to distinguish two very different decode failures:
//
//   - A malformed observation (bad framing, bad proto, unmarshalable blob
//     payload) is deterministic across oracles — every correct oracle sees the
//     same bytes — so it is safe to drop that single observation.
//   - A blob-fetch failure is node-local and possibly transient: one oracle may
//     fail to fetch while others succeed. Dropping the observation on only some
//     oracles would make StateTransition non-deterministic and could halt the
//     protocol. Callers inside StateTransition must propagate this (aborting and
//     uniformly retrying the round) rather than skipping the observation.
type blobFetchError struct{ err error }

func (e *blobFetchError) Error() string { return e.err.Error() }
func (e *blobFetchError) Unwrap() error { return e.err }

// decodeObservation reverses encodeObservation, fetching any referenced blobs.
// Observations carrying inline stream values are rejected: v31 disseminates
// values exclusively via blobs.
//
// memo, when non-nil, memoizes decoded blob payloads for the round so the same
// handle is fetched and decompressed once instead of once per plugin phase. It
// changes cost only: an observation decodes identically on a hit and on a miss.
func decodeObservation(ctx context.Context, raw ocrtypes.Observation, bf ocr3_1types.BlobFetcher, memo *roundBlobPayloads) (Observation, error) {
	if len(raw) == 0 {
		return Observation{}, nil
	}
	if raw[0] != observationWireVersion {
		return Observation{}, fmt.Errorf("unknown observation wire version %d", raw[0])
	}
	rest := raw[1:]
	nHandles, k := binary.Uvarint(rest)
	if k <= 0 {
		return Observation{}, fmt.Errorf("malformed observation: bad handle count")
	}
	if nHandles > maxObservationBlobHandles {
		return Observation{}, fmt.Errorf("observation references too many blobs: %d (max %d)", nHandles, maxObservationBlobHandles)
	}
	rest = rest[k:]

	// The marshaled bytes are kept alongside the decoded handle: they are the
	// memo key, and re-marshaling to obtain it would be wasted work.
	type blobRef struct {
		handle ocr3_1types.BlobHandle
		key    []byte
	}
	handles := make([]blobRef, 0, nHandles)
	for i := uint64(0); i < nHandles; i++ {
		l, k2 := binary.Uvarint(rest)
		if k2 <= 0 || uint64(len(rest[k2:])) < l {
			return Observation{}, fmt.Errorf("malformed observation: bad handle length")
		}
		rest = rest[k2:]
		var h ocr3_1types.BlobHandle
		if err := h.UnmarshalBinary(rest[:l]); err != nil {
			return Observation{}, fmt.Errorf("unmarshal blob handle: %w", err)
		}
		handles = append(handles, blobRef{handle: h, key: rest[:l]})
		rest = rest[l:]
	}

	main := &protocol.LLOObservationProto{}
	if err := proto.Unmarshal(rest, main); err != nil {
		return Observation{}, fmt.Errorf("unmarshal observation: %w", err)
	}

	obs, err := observationFromProto(main)
	if err != nil {
		return Observation{}, err
	}

	// Fetch and merge blob-carried stream values. The decompression budget is
	// shared across handles, so a peer cannot multiply the work it imposes by
	// naming several blobs.
	budget := maxObservationDecompressedBytes
	for _, h := range handles {
		entry, ok := memo.get(h.key)
		if ok {
			// A hit is charged the same decompressed size the miss was, so a
			// handle named twice exhausts the budget exactly as before.
			if entry.size > budget {
				return Observation{}, fmt.Errorf("decompressed blob payload too large: %d > %d bytes", entry.size, budget)
			}
		} else {
			if bf == nil {
				return Observation{}, &blobFetchError{fmt.Errorf("observation references a blob but no fetcher was provided")}
			}
			payload, ferr := bf.FetchBlob(ctx, h.handle)
			if ferr != nil {
				return Observation{}, &blobFetchError{fmt.Errorf("fetch blob: %w", ferr)}
			}
			// Framing/codec faults are deterministic across oracles (every one sees
			// the same bytes), so they stay plain errors and drop this observation
			// alone, unlike the fetch failure above.
			raw, err := decodeBlobPayload(payload, budget)
			if err != nil {
				return Observation{}, err
			}
			chunk := &protocol.LLOObservationProto{}
			if err := proto.Unmarshal(raw, chunk); err != nil {
				return Observation{}, fmt.Errorf("unmarshal blob payload: %w", err)
			}
			values := make(protocol.StreamValues, len(chunk.StreamValues))
			for id, pbSv := range chunk.StreamValues {
				sv, err := streamValueFromProtoAllowNil(pbSv)
				if err != nil {
					return Observation{}, err
				}
				values[id] = sv
			}
			// Only a payload that decoded cleanly is memoized. A fetch failure is
			// node-local and transient, and a decode failure is deterministic and
			// recomputed for free, so neither is worth remembering.
			entry = blobPayloadEntry{values: values, size: len(raw)}
			memo.put(h.key, entry)
		}

		budget -= entry.size
		if obs.StreamValues == nil {
			obs.StreamValues = make(protocol.StreamValues, len(entry.values))
		}
		for id, sv := range entry.values {
			obs.StreamValues[id] = sv
		}
	}

	return obs, nil
}

func observationFromProto(main *protocol.LLOObservationProto) (Observation, error) {
	obs := Observation{
		AttestedPredecessorRetirement: main.AttestedPredecessorRetirement,
		ShouldRetire:                  main.ShouldRetire,
		UnixTimestampNanoseconds:      main.UnixTimestampNanoseconds,
	}
	if len(main.RemoveChannelIDs) > 0 {
		obs.RemoveChannelIDs = make(map[llotypes.ChannelID]struct{}, len(main.RemoveChannelIDs))
		for _, id := range main.RemoveChannelIDs {
			obs.RemoveChannelIDs[id] = struct{}{}
		}
	}
	if len(main.UpdateChannelDefinitions) > 0 {
		obs.UpdateChannelDefinitions = make(llotypes.ChannelDefinitions, len(main.UpdateChannelDefinitions))
		for id, pb := range main.UpdateChannelDefinitions {
			if pb == nil {
				return Observation{}, fmt.Errorf("nil channel definition for channel %d", id)
			}
			obs.UpdateChannelDefinitions[id] = protocol.ChannelDefinitionFromProto(pb)
		}
	}
	// v31 carries stream values exclusively in blobs. A conforming encoder never
	// populates this field, so its presence means the peer is not speaking v31
	// framing; reject rather than silently accepting an out-of-band path around
	// blob dissemination. This is deterministic across oracles (all see the same
	// bytes), so dropping the observation is safe.
	if len(main.StreamValues) > 0 {
		return Observation{}, fmt.Errorf("observation carries %d inline stream values: v31 requires blob-carried values", len(main.StreamValues))
	}
	if len(main.SupportedReportFormats) > protocol.MaxObservationSupportedReportFormatsLength {
		return Observation{}, fmt.Errorf("observation advertises too many report formats: %d (max %d)", len(main.SupportedReportFormats), protocol.MaxObservationSupportedReportFormatsLength)
	}

	if len(main.PredecessorSigners) > protocol.MaxObservationPredecessorSignersLength {
		return Observation{}, fmt.Errorf("observation carries too many predecessor signers: %d (max %d)", len(main.PredecessorSigners), protocol.MaxObservationPredecessorSignersLength)
	}
	for i, signer := range main.PredecessorSigners {
		if len(signer) == 0 || len(signer) > protocol.MaxPredecessorSignerBytes {
			return Observation{}, fmt.Errorf("observation carries predecessor signer %d of invalid length %d (max %d)", i, len(signer), protocol.MaxPredecessorSignerBytes)
		}
	}
	// f indexes nothing, but a set that cannot reach f+1 valid signatures could
	// never verify a report, so treat it as malformed rather than carrying it
	// into the vote.
	if main.PredecessorF > 0 && int(main.PredecessorF) >= len(main.PredecessorSigners) {
		return Observation{}, fmt.Errorf("observation carries predecessor f=%d for a signer set of %d", main.PredecessorF, len(main.PredecessorSigners))
	}
	obs.PredecessorSigners = main.PredecessorSigners
	obs.PredecessorF = uint8(main.PredecessorF)

	obs.SupportedReportFormats = formatsFromWire(main.SupportedReportFormats)
	return obs, nil
}

// sortedFormatsToWire returns the formats sorted ascending. nil in, nil out, so
// an oracle advertising nothing stays absent from the wire rather than carrying
// an empty list.
func sortedFormatsToWire(in map[llotypes.ReportFormat]struct{}) []uint32 {
	if len(in) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(in))
	for f := range in {
		out = append(out, uint32(f))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// formatsFromWire is sortedFormatsToWire inverted. Duplicate wire entries
// collapse, so the set an oracle advertises never depends on repetition.
func formatsFromWire(in []uint32) map[llotypes.ReportFormat]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[llotypes.ReportFormat]struct{}, len(in))
	for _, f := range in {
		out[llotypes.ReportFormat(f)] = struct{}{}
	}
	return out
}

func streamValuesToProto(in protocol.StreamValues) (map[uint32]*protocol.LLOStreamValue, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[uint32]*protocol.LLOStreamValue, len(in))
	for id, sv := range in {
		if sv == nil {
			// Unobserved stream; skip (matches v30 semantics of not setting a value).
			continue
		}
		pb, err := protocol.StreamValueToProto(sv)
		if err != nil {
			return nil, fmt.Errorf("stream %d: %w", id, err)
		}
		out[id] = pb
	}
	return out, nil
}

// streamValueFromProtoAllowNil decodes a possibly-nil stream value proto,
// returning a nil StreamValue for a nil proto (an unobserved stream).
func streamValueFromProtoAllowNil(pb *protocol.LLOStreamValue) (protocol.StreamValue, error) {
	if pb == nil {
		return nil, nil
	}
	return protocol.UnmarshalObservedProtoStreamValue(pb)
}
