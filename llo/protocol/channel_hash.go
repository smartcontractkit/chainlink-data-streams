package protocol

import "crypto/sha256"

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
	b, err := deterministicMarshal.Marshal(pb)
	if err != nil {
		// Marshaling a well-formed definition cannot fail; hash empty on the
		// impossible error path rather than panicking.
		return sha256.Sum256(nil)
	}
	return sha256.Sum256(b)
}
