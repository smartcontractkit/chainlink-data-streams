package llo

import (
	"bytes"
	"sync"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// previousChannelDefinitions memoizes the channel definitions of the last
// previous outcome ValidateObservation decoded. Every update carrying
// observation of a round is validated against the same previous outcome, and a
// follower validates them serially before Outcome, so decoding it once per
// round instead of once per observation shortens the round.
//
// A hit compares the encoded outcome byte for byte, so it never serves the
// definitions of a different outcome. The zero value is ready to use.
type previousChannelDefinitions struct {
	mu      sync.Mutex
	valid   bool
	outcome []byte
	defs    llotypes.ChannelDefinitions
}

// get returns the channel definitions of the encoded outcome, decoding it only
// when it differs from the last one. The lock is held while decoding, so
// concurrent callers with the same outcome wait for one decode rather than
// each running their own. The returned map is shared and must not be modified.
func (m *previousChannelDefinitions) get(codec OutcomeCodec, outcome []byte) (llotypes.ChannelDefinitions, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.valid && bytes.Equal(m.outcome, outcome) {
		return m.defs, nil
	}
	decoded, err := codec.Decode(outcome)
	if err != nil {
		return nil, err
	}
	m.valid = true
	m.outcome = bytes.Clone(outcome)
	m.defs = decoded.ChannelDefinitions
	return m.defs, nil
}
