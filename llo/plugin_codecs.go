package llo

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"golang.org/x/exp/maps"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"google.golang.org/protobuf/proto"
)

// NOTE: These codecs make a lot of allocations which will be hard on the
// garbage collector, this can probably be made more efficient

// OBSERVATION CODEC

var (
	_ ObservationCodec = (*protoObservationCodec)(nil)
)

type ObservationCodec interface {
	Encode(obs Observation) (types.Observation, error)
	Decode(encoded types.Observation) (obs Observation, err error)
}

type protoObservationCodec struct{}

func (c protoObservationCodec) Encode(obs Observation) (types.Observation, error) {
	dfns := channelDefinitionsToProtoObservation(obs.UpdateChannelDefinitions)

	streamValues := make(map[uint32][]byte, len(obs.StreamValues))
	for id, sv := range obs.StreamValues {
		if sv != nil {
			streamValues[id] = sv.Bytes()
		}
	}

	pbuf := &LLOObservationProto{
		AttestedPredecessorRetirement: obs.AttestedPredecessorRetirement,
		ShouldRetire:                  obs.ShouldRetire,
		UnixTimestampNanoseconds:      obs.UnixTimestampNanoseconds,
		RemoveChannelIDs:              maps.Keys(obs.RemoveChannelIDs),
		UpdateChannelDefinitions:      dfns,
		StreamValues:                  streamValues,
	}

	return proto.Marshal(pbuf)
}

func channelDefinitionsToProtoObservation(in llotypes.ChannelDefinitions) (out map[uint32]*LLOChannelDefinitionProto) {
	if len(in) > 0 {
		out = make(map[uint32]*LLOChannelDefinitionProto, len(in))
		for id, d := range in {
			out[id] = &LLOChannelDefinitionProto{
				ReportFormat:  uint32(d.ReportFormat),
				ChainSelector: d.ChainSelector,
				StreamIDs:     d.StreamIDs,
			}
		}
	}
	return
}

// TODO: Guard against untrusted inputs!
func (c protoObservationCodec) Decode(b types.Observation) (Observation, error) {
	pbuf := &LLOObservationProto{}
	err := proto.Unmarshal(b, pbuf)
	if err != nil {
		return Observation{}, fmt.Errorf("failed to decode observation: expected protobuf (got: 0x%x); %w", b, err)
	}
	var removeChannelIDs map[llotypes.ChannelID]struct{}
	if len(pbuf.RemoveChannelIDs) > 0 {
		removeChannelIDs = make(map[llotypes.ChannelID]struct{}, len(pbuf.RemoveChannelIDs))
		for _, id := range pbuf.RemoveChannelIDs {
			removeChannelIDs[id] = struct{}{}
		}
	}
	dfns := channelDefinitionsFromProtoObservation(pbuf.UpdateChannelDefinitions)
	var streamValues StreamValues
	if len(pbuf.StreamValues) > 0 {
		streamValues = make(StreamValues, len(pbuf.StreamValues))
		for id, sv := range pbuf.StreamValues {
			// StreamValues shouldn't have explicit nils, but for safety we
			// ought to handle it anyway
			if sv != nil {
				streamValues[id] = new(big.Int).SetBytes(sv)
			}
		}
	}
	obs := Observation{
		AttestedPredecessorRetirement: pbuf.AttestedPredecessorRetirement,
		ShouldRetire:                  pbuf.ShouldRetire,
		UnixTimestampNanoseconds:      pbuf.UnixTimestampNanoseconds,
		RemoveChannelIDs:              removeChannelIDs,
		UpdateChannelDefinitions:      dfns,
		StreamValues:                  streamValues,
	}
	return obs, nil
}

func channelDefinitionsFromProtoObservation(channelDefinitions map[uint32]*LLOChannelDefinitionProto) llotypes.ChannelDefinitions {
	if len(channelDefinitions) == 0 {
		return nil
	}
	dfns := make(map[llotypes.ChannelID]llotypes.ChannelDefinition, len(channelDefinitions))
	for id, d := range channelDefinitions {
		dfns[id] = llotypes.ChannelDefinition{
			ReportFormat:  llotypes.ReportFormat(d.ReportFormat),
			ChainSelector: d.ChainSelector,
			StreamIDs:     d.StreamIDs,
		}
	}
	return dfns
}

// OUTCOME CODEC

var _ OutcomeCodec = (*protoOutcomeCodec)(nil)

type OutcomeCodec interface {
	Encode(outcome Outcome) (ocr3types.Outcome, error)
	Decode(encoded ocr3types.Outcome) (outcome Outcome, err error)
}

type protoOutcomeCodec struct{}

func (protoOutcomeCodec) Encode(outcome Outcome) (ocr3types.Outcome, error) {
	dfns := channelDefinitionsToProtoOutcome(outcome.ChannelDefinitions)

	streamMedians := streamMediansToProtoOutcome(outcome.StreamMedians)
	validAfterSeconds := validAfterSecondsToProtoOutcome(outcome.ValidAfterSeconds)

	pbuf := &LLOOutcomeProto{
		LifeCycleStage:                   string(outcome.LifeCycleStage),
		ObservationsTimestampNanoseconds: outcome.ObservationsTimestampNanoseconds,
		ChannelDefinitions:               dfns,
		ValidAfterSeconds:                validAfterSeconds,
		StreamMedians:                    streamMedians,
	}

	// It's very important that Outcome serialization be deterministic across all nodes!
	// Should be reliable since we don't use maps
	return proto.MarshalOptions{Deterministic: true}.Marshal(pbuf)
}

func channelDefinitionsToProtoOutcome(in llotypes.ChannelDefinitions) (out []*LLOChannelIDAndDefinitionProto) {
	if len(in) > 0 {
		out = make([]*LLOChannelIDAndDefinitionProto, 0, len(in))
		for id, d := range in {
			out = append(out, &LLOChannelIDAndDefinitionProto{
				ChannelID: id,
				ChannelDefinition: &LLOChannelDefinitionProto{
					ReportFormat:  uint32(d.ReportFormat),
					ChainSelector: d.ChainSelector,
					StreamIDs:     d.StreamIDs,
				},
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].ChannelID < out[j].ChannelID
		})
	}
	return
}

func streamMediansToProtoOutcome(in StreamValues) (out []*LLOStreamIDAndValue) {
	if len(in) > 0 {
		out = make([]*LLOStreamIDAndValue, 0, len(in))
		for id, v := range in {
			// StreamMedians shouldn't have explicit nil values, but for
			// safety we ought to handle it anyway
			if v != nil {
				out = append(out, &LLOStreamIDAndValue{
					StreamID: id,
					Value:    v.Bytes(),
				})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].StreamID < out[j].StreamID
		})
	}
	return
}

func validAfterSecondsToProtoOutcome(in map[llotypes.ChannelID]uint32) (out []*LLOChannelIDAndValidAfterSecondsProto) {
	if len(in) > 0 {
		out = make([]*LLOChannelIDAndValidAfterSecondsProto, 0, len(in))
		for id, v := range in {
			out = append(out, &LLOChannelIDAndValidAfterSecondsProto{
				ChannelID:         id,
				ValidAfterSeconds: v,
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].ChannelID < out[j].ChannelID
		})
	}
	return
}

// TODO: Guard against untrusted inputs!
func (protoOutcomeCodec) Decode(b ocr3types.Outcome) (outcome Outcome, err error) {
	pbuf := &LLOOutcomeProto{}
	err = proto.Unmarshal(b, pbuf)
	if err != nil {
		return Outcome{}, fmt.Errorf("failed to decode outcome: expected protobuf (got: 0x%x); %w", b, err)
	}
	dfns := channelDefinitionsFromProtoOutcome(pbuf.ChannelDefinitions)
	streamMedians := streamMediansFromProtoOutcome(pbuf.StreamMedians)
	validAfterSeconds := validAfterSecondsFromProtoOutcome(pbuf.ValidAfterSeconds)
	outcome = Outcome{
		LifeCycleStage:                   llotypes.LifeCycleStage(pbuf.LifeCycleStage),
		ObservationsTimestampNanoseconds: pbuf.ObservationsTimestampNanoseconds,
		ChannelDefinitions:               dfns,
		ValidAfterSeconds:                validAfterSeconds,
		StreamMedians:                    streamMedians,
	}
	return outcome, nil
}

func channelDefinitionsFromProtoOutcome(in []*LLOChannelIDAndDefinitionProto) (out llotypes.ChannelDefinitions) {
	if len(in) > 0 {
		out = make(map[llotypes.ChannelID]llotypes.ChannelDefinition, len(in))
		for _, d := range in {
			out[d.ChannelID] = llotypes.ChannelDefinition{
				ReportFormat:  llotypes.ReportFormat(d.ChannelDefinition.ReportFormat),
				ChainSelector: d.ChannelDefinition.ChainSelector,
				StreamIDs:     d.ChannelDefinition.StreamIDs,
			}
		}
	}
	return
}

func streamMediansFromProtoOutcome(in []*LLOStreamIDAndValue) (out StreamValues) {
	if len(in) > 0 {
		out = make(map[llotypes.StreamID]*big.Int, len(in))
		for _, sv := range in {
			if sv.Value != nil {
				// StreamMedians shouldn't have explicit nil values, but for
				// safety we ought to handle it anyway
				out[sv.StreamID] = new(big.Int).SetBytes(sv.Value)
			}
		}
	}
	return
}

func validAfterSecondsFromProtoOutcome(in []*LLOChannelIDAndValidAfterSecondsProto) (out map[llotypes.ChannelID]uint32) {
	if len(in) > 0 {
		out = make(map[llotypes.ChannelID]uint32, len(in))
		for _, v := range in {
			out[v.ChannelID] = v.ValidAfterSeconds
		}
	}
	return
}
