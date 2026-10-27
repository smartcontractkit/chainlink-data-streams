package llo

import (
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// tickOverlap reports whether validAfterNs and obsTsNs collapse to the same
// (or an inverted) tick at res. It truncates with protocol.ConvertTimestamp,
// the same conversion the codecs apply when encoding. A channel must not emit
// a report whose validAfter and observation timestamp fall in the same tick,
// otherwise the encoded report would be not yet valid (ValidFrom after
// Timestamp).
func tickOverlap(validAfterNs, obsTsNs uint64, res protocol.TimeResolution) bool {
	return protocol.ConvertTimestamp(validAfterNs, res) >= protocol.ConvertTimestamp(obsTsNs, res)
}
