package llo

import (
	"context"
	"encoding/binary"
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

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
	StreamValues                  llocommon.StreamValues
}

// observationWireVersion is the leading byte of the v31 observation framing.
const observationWireVersion byte = 1

// maxObservationBlobHandles bounds the number of blob handles a single
// observation may reference. The encoder emits at most one; this is a generous
// ceiling that prevents a malicious peer from forcing a huge allocation via a
// crafted handle count in decodeObservation.
const maxObservationBlobHandles = 64

// encodeObservation serializes an Observation. When the serialized stream-value
// payload exceeds blobThreshold, the stream values are broadcast as a blob and
// referenced by handle instead of being sent inline. A threshold of 0 disables
// blob offloading. A nil broadcaster also disables offloading (values inline).
func encodeObservation(ctx context.Context, obs Observation, seqNr uint64, blobThreshold int, bbf ocr3_1types.BlobBroadcaster) (ocrtypes.Observation, error) {
	main := &llocommon.LLOObservationProto{
		AttestedPredecessorRetirement: obs.AttestedPredecessorRetirement,
		ShouldRetire:                  obs.ShouldRetire,
		UnixTimestampNanoseconds:      obs.UnixTimestampNanoseconds,
	}
	for id := range obs.RemoveChannelIDs {
		main.RemoveChannelIDs = append(main.RemoveChannelIDs, id)
	}
	if len(obs.UpdateChannelDefinitions) > 0 {
		main.UpdateChannelDefinitions = make(map[uint32]*llocommon.LLOChannelDefinitionProto, len(obs.UpdateChannelDefinitions))
		for id, cd := range obs.UpdateChannelDefinitions {
			main.UpdateChannelDefinitions[id] = makeChannelDefinitionProto(cd)
		}
	}

	streamValues, err := streamValuesToProto(obs.StreamValues)
	if err != nil {
		return nil, err
	}

	// Decide whether to offload stream values to a blob.
	var handles [][]byte
	if len(streamValues) > 0 {
		svOnly := &llocommon.LLOObservationProto{StreamValues: streamValues}
		svBytes, err := proto.Marshal(svOnly)
		if err != nil {
			return nil, fmt.Errorf("marshal stream values: %w", err)
		}
		if blobThreshold > 0 && bbf != nil && len(svBytes) > blobThreshold {
			handle, berr := bbf.BroadcastBlob(ctx, svBytes, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: seqNr + 1})
			if berr != nil {
				// Blob broadcast failure must not fail Observation; fall back to inline.
				main.StreamValues = streamValues
			} else {
				hb, herr := handle.MarshalBinary()
				if herr != nil {
					return nil, fmt.Errorf("marshal blob handle: %w", herr)
				}
				handles = append(handles, hb)
			}
		} else {
			main.StreamValues = streamValues
		}
	}

	mainBytes, err := proto.Marshal(main)
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
func decodeObservation(ctx context.Context, raw ocrtypes.Observation, bf ocr3_1types.BlobFetcher) (Observation, error) {
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

	handles := make([]ocr3_1types.BlobHandle, 0, nHandles)
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
		handles = append(handles, h)
		rest = rest[l:]
	}

	main := &llocommon.LLOObservationProto{}
	if err := proto.Unmarshal(rest, main); err != nil {
		return Observation{}, fmt.Errorf("unmarshal observation: %w", err)
	}

	obs, err := observationFromProto(main)
	if err != nil {
		return Observation{}, err
	}

	// Fetch and merge blob-carried stream values.
	for _, h := range handles {
		if bf == nil {
			return Observation{}, &blobFetchError{fmt.Errorf("observation references a blob but no fetcher was provided")}
		}
		payload, ferr := bf.FetchBlob(ctx, h)
		if ferr != nil {
			return Observation{}, &blobFetchError{fmt.Errorf("fetch blob: %w", ferr)}
		}
		chunk := &llocommon.LLOObservationProto{}
		if err := proto.Unmarshal(payload, chunk); err != nil {
			return Observation{}, fmt.Errorf("unmarshal blob payload: %w", err)
		}
		if obs.StreamValues == nil {
			obs.StreamValues = make(llocommon.StreamValues, len(chunk.StreamValues))
		}
		for id, pbSv := range chunk.StreamValues {
			sv, err := streamValueFromProtoAllowNil(pbSv)
			if err != nil {
				return Observation{}, err
			}
			obs.StreamValues[id] = sv
		}
	}

	return obs, nil
}

func observationFromProto(main *llocommon.LLOObservationProto) (Observation, error) {
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
			obs.UpdateChannelDefinitions[id] = channelDefinitionFromProto(pb)
		}
	}
	if len(main.StreamValues) > 0 {
		obs.StreamValues = make(llocommon.StreamValues, len(main.StreamValues))
		for id, pbSv := range main.StreamValues {
			sv, err := streamValueFromProtoAllowNil(pbSv)
			if err != nil {
				return Observation{}, err
			}
			obs.StreamValues[id] = sv
		}
	}
	return obs, nil
}

func streamValuesToProto(in llocommon.StreamValues) (map[uint32]*llocommon.LLOStreamValue, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[uint32]*llocommon.LLOStreamValue, len(in))
	for id, sv := range in {
		if sv == nil {
			// Unobserved stream; skip (matches v30 semantics of not setting a value).
			continue
		}
		pb, err := makeLLOStreamValue(sv)
		if err != nil {
			return nil, fmt.Errorf("stream %d: %w", id, err)
		}
		out[id] = pb
	}
	return out, nil
}

// streamValueFromProtoAllowNil decodes a possibly-nil stream value proto,
// returning a nil StreamValue for a nil proto (an unobserved stream).
func streamValueFromProtoAllowNil(pb *llocommon.LLOStreamValue) (llocommon.StreamValue, error) {
	if pb == nil {
		return nil, nil
	}
	return llocommon.UnmarshalProtoStreamValue(pb)
}
