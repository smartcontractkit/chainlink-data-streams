package protocol

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_ChannelHashV2(t *testing.T) {
	base := ChannelDefinitionWithID{
		ChannelID: 1,
		ChannelDefinition: llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormat(1),
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}},
			Opts:         []byte(`{}`),
		},
	}

	t.Run("is stable", func(t *testing.T) {
		// NOTE: Breaking this test by changing the hash below breaks both v3.1
		// and any v3.0 instance running protocol version 2. It is a protocol
		// change, not a refactor.
		assert.Equal(t, "f0c21393ace2c693200c2a1a61ab915b767a0964263ccea2a86e124a664f9904", fmt.Sprintf("%x", ChannelHashV2(base)))
	})

	t.Run("commits to every field Equals compares", func(t *testing.T) {
		// This is the property the v0/v1 hash lacked for the last three cases.
		for name, mutate := range map[string]func(*ChannelDefinitionWithID){
			"channelID":              func(d *ChannelDefinitionWithID) { d.ChannelID = 2 },
			"reportFormat":           func(d *ChannelDefinitionWithID) { d.ReportFormat = llotypes.ReportFormat(2) },
			"streamID":               func(d *ChannelDefinitionWithID) { d.Streams[0].StreamID = 99 },
			"aggregator":             func(d *ChannelDefinitionWithID) { d.Streams[0].Aggregator = llotypes.AggregatorQuote },
			"streamCount":            func(d *ChannelDefinitionWithID) { d.Streams = d.Streams[:1] },
			"opts":                   func(d *ChannelDefinitionWithID) { d.Opts = []byte(`{"foo":"bar"}`) },
			"tombstone":              func(d *ChannelDefinitionWithID) { d.Tombstone = true },
			"source":                 func(d *ChannelDefinitionWithID) { d.Source = 7 },
			"disableNilStreamValues": func(d *ChannelDefinitionWithID) { d.DisableNilStreamValues = true },
		} {
			t.Run(name, func(t *testing.T) {
				mutated := base
				mutated.Streams = append([]llotypes.Stream(nil), base.Streams...)
				mutate(&mutated)

				assert.False(t, base.Equals(mutated.ChannelDefinition) && base.ChannelID == mutated.ChannelID,
					"test bug: mutation did not actually change the definition")
				assert.NotEqual(t, ChannelHashV2(base), ChannelHashV2(mutated))
			})
		}
	})

	t.Run("is stable across repeated marshaling", func(t *testing.T) {
		for range 100 {
			assert.Equal(t, ChannelHashV2(base), ChannelHashV2(base))
		}
	})
}
