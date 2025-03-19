package llo

import (
	"bytes"
	reflect "reflect"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"golang.org/x/exp/maps"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func genLifecycleStage() gopter.Gen {
	return gen.AnyString().Map(func(s string) llotypes.LifeCycleStage {
		return llotypes.LifeCycleStage(s)
	})
}

func genAttestedPredecessorRetirement() gopter.Gen {
	return gen.SliceOf(gen.UInt8())
}

func genRemoveChannelIDs() gopter.Gen {
	return gen.MapOf(gen.UInt32(), gen.Const(struct{}{}))
}

func genChannelDefinitions() gopter.Gen {
	return gen.MapOf(gen.UInt32(), genChannelDefinition())
}

func genStreamAggregates() gopter.Gen {
	return gen.MapOf(gen.UInt32(), genMapOfAggregatorStreamValue()).Map(func(m map[uint32]map[uint32]StreamValue) map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue {
		m2 := make(map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue)
		for k, v := range m {
			m3 := make(map[llotypes.Aggregator]StreamValue)
			for k2, v2 := range v {
				m3[llotypes.Aggregator(k2)] = v2
			}
			m2[k] = m3
		}
		return m2
	})
}

func genMapOfAggregatorStreamValue() gopter.Gen {
	return genStreamValuesMap()
}

func genStreamValuesMap() gopter.Gen {
	return genStreamValues(true).Map(func(values []StreamValue) map[llotypes.StreamID]StreamValue {
		m := make(map[llotypes.StreamID]StreamValue)
		for i, v := range values {
			m[llotypes.StreamID(i)] = v // nolint:gosec // don't care if it overflows
		}
		return m
	})
}

func genChannelDefinition() gopter.Gen {
	return gen.StrictStruct(reflect.TypeOf(llotypes.ChannelDefinition{}), map[string]gopter.Gen{
		"ReportFormat": genReportFormat(),
		"Streams":      gen.SliceOf(genStream()),
		"Opts":         gen.SliceOf(gen.UInt8()),
	})
}

func genReportFormat() gopter.Gen {
	return gen.UInt32().Map(func(i uint32) llotypes.ReportFormat {
		return llotypes.ReportFormat(i)
	})
}

func genStream() gopter.Gen {
	return gen.StrictStruct(reflect.TypeOf(llotypes.Stream{}), map[string]gopter.Gen{
		"StreamID":   gen.UInt32(),
		"Aggregator": genAggregator(),
	})
}

func genAggregator() gopter.Gen {
	return gen.UInt32().Map(func(i uint32) llotypes.Aggregator {
		return llotypes.Aggregator(i)
	})
}

func equalObservations(obs, obs2 Observation) bool {
	if !bytes.Equal(obs.AttestedPredecessorRetirement, obs2.AttestedPredecessorRetirement) {
		return false
	}
	if obs.ShouldRetire != obs2.ShouldRetire {
		return false
	}
	if obs.UnixTimestampNanoseconds != obs2.UnixTimestampNanoseconds {
		return false
	}
	if len(obs.RemoveChannelIDs) != len(obs2.RemoveChannelIDs) {
		return false
	}
	for k := range obs.RemoveChannelIDs {
		if _, ok := obs2.RemoveChannelIDs[k]; !ok {
			return false
		}
	}

	if len(obs.UpdateChannelDefinitions) != len(obs2.UpdateChannelDefinitions) {
		return false
	}
	for k, v := range obs.UpdateChannelDefinitions {
		v2, ok := obs2.UpdateChannelDefinitions[k]
		if !ok {
			return false
		}
		if v.ReportFormat != v2.ReportFormat {
			return false
		}
		if !reflect.DeepEqual(v.Streams, v2.Streams) {
			return false
		}
		if !bytes.Equal(v.Opts, v2.Opts) {
			return false
		}
	}

	if len(obs.StreamValues) != len(obs2.StreamValues) {
		return false
	}
	for k, v := range obs.StreamValues {
		v2, ok := obs2.StreamValues[k]
		if !ok {
			return false
		}
		if !equalStreamValues(v, v2) {
			return false
		}
	}
	return true
}

func equalOutcomes(t *testing.T, outcome, outcome2 Outcome) bool {
	if outcome.LifeCycleStage != outcome2.LifeCycleStage {
		t.Logf("Outcomes not equal; LifeCycleStage: %v != %v", outcome.LifeCycleStage, outcome2.LifeCycleStage)
		return false
	}
	if outcome.ObservationTimestampNanoseconds != outcome2.ObservationTimestampNanoseconds {
		t.Logf("Outcomes not equal; ObservationTimestampNanoseconds: %v != %v", outcome.ObservationTimestampNanoseconds, outcome2.ObservationTimestampNanoseconds)
		return false
	}
	if len(outcome.ChannelDefinitions) != len(outcome2.ChannelDefinitions) {
		t.Logf("Outcomes not equal; ChannelDefinitions: %v != %v", outcome.ChannelDefinitions, outcome2.ChannelDefinitions)
		return false
	}
	for k, v := range outcome.ChannelDefinitions {
		v2, ok := outcome2.ChannelDefinitions[k]
		if !ok {
			t.Logf("Outcomes not equal; ChannelDefinitions: %v != %v", outcome.ChannelDefinitions, outcome2.ChannelDefinitions)
			return false
		}
		if v.ReportFormat != v2.ReportFormat {
			t.Logf("Outcomes not equal; ChannelDefinitions: %v != %v", outcome.ChannelDefinitions, outcome2.ChannelDefinitions)
			return false
		}
		if !reflect.DeepEqual(v.Streams, v2.Streams) {
			t.Logf("Outcomes not equal; ChannelDefinitions: %v != %v", outcome.ChannelDefinitions, outcome2.ChannelDefinitions)
			return false
		}
		if !bytes.Equal(v.Opts, v2.Opts) {
			t.Logf("Outcomes not equal; ChannelDefinitions: %v != %v", outcome.ChannelDefinitions, outcome2.ChannelDefinitions)
			return false
		}
	}

	if len(outcome.ValidAfterNanoseconds) != len(outcome2.ValidAfterNanoseconds) {
		t.Logf("Outcomes not equal; ValidAfterNanoseconds: %v != %v", outcome.ValidAfterNanoseconds, outcome2.ValidAfterNanoseconds)
		return false
	}
	for k, v := range outcome.ValidAfterNanoseconds {
		v2, ok := outcome2.ValidAfterNanoseconds[k]
		if !ok {
			t.Logf("Outcomes not equal; ValidAfterNanoseconds: %v != %v", outcome.ValidAfterNanoseconds, outcome2.ValidAfterNanoseconds)
			return false
		}
		if v != v2 {
			t.Logf("Outcomes not equal; ValidAfterNanoseconds: %v != %v", outcome.ValidAfterNanoseconds, outcome2.ValidAfterNanoseconds)
			return false
		}
	}

	// filter out nils
	saggs1 := maps.Clone(outcome.StreamAggregates)
	saggs2 := maps.Clone(outcome2.StreamAggregates)
	for k, v := range saggs1 {
		if len(v) == 0 {
			delete(saggs1, k)
		}
	}
	for k, v := range saggs2 {
		if len(v) == 0 {
			delete(saggs2, k)
		}
	}

	if len(saggs1) != len(saggs2) {
		t.Logf("Outcomes not equal; StreamAggregates: %v != %v", saggs1, saggs2)
		return false
	}
	for k, v := range saggs1 {
		v2, ok := saggs2[k]
		if !ok {
			t.Logf("Outcomes not equal; StreamAggregates: %v != %v", saggs1, saggs2)
			return false
		}
		if !equalStreamAggregates(v, v2) {
			t.Logf("Outcomes not equal; StreamAggregates: %v != %v", saggs1, saggs2)
			return false
		}
	}
	return true
}

func equalStreamAggregates(m1, m2 map[llotypes.Aggregator]StreamValue) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v := range m1 {
		v2, ok := m2[k]
		if !ok {
			return false
		}
		if !equalStreamValues(v, v2) {
			return false
		}
	}
	return true
}
