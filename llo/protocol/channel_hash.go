package protocol

import (
	"crypto/sha256"

	"google.golang.org/protobuf/proto"
)

// DeterministicMarshal marshals a proto message deterministically. Anything
// whose bytes must agree across oracles has to go through this rather than
// plain proto.Marshal, whose map and unknown-field ordering is unspecified.
var DeterministicMarshal = proto.MarshalOptions{Deterministic: true}

// ChannelHashV2 is the channel definition identity that consensus votes are
// counted under from LLO protocol version 2 onwards, and the only channel hash
// v3.1 has ever used.
//
// This function is shared by v3.0 (protocol version 2) and v3.1 precisely so
// the two cannot drift. Changing it is a breaking protocol change for both and
// must be gated on a new protocol version, never applied in place.
func ChannelHashV2(cd ChannelDefinitionWithID) ChannelHash {
	pb := &LLOChannelIDAndDefinitionProto{
		ChannelID:         cd.ChannelID,
		ChannelDefinition: ChannelDefinitionToProto(cd.ChannelDefinition),
	}
	b, err := DeterministicMarshal.Marshal(pb)
	if err != nil {
		// Marshaling a well-formed definition cannot fail; hash empty on the
		// impossible error path rather than panicking.
		return sha256.Sum256(nil)
	}
	return sha256.Sum256(b)
}
