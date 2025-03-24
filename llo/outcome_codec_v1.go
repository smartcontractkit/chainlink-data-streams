package llo

import (
	"fmt"
	"sort"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// OUTCOME CODEC
// NOTE: These codecs make a lot of allocations which will be hard on the
// garbage collector, this can probably be made more efficient

var _ OutcomeCodec = (*protoOutcomeCodecV1)(nil)

type protoOutcomeCodecV1 struct{}

func (protoOutcomeCodecV1) Encode(outcome Outcome) (ocr3types.Outcome, error) {
	dfns := channelDefinitionsToProtoOutcome(outcome.ChannelDefinitions)

	streamAggregates, err := StreamAggregatesToProtoOutcome(outcome.StreamAggregates)
	if err != nil {
		return nil, err
	}

	validAfterNanoseconds := validAfterNanosecondsToProtoOutcomeNanoseconds(outcome.ValidAfterNanoseconds)

	pbuf := &LLOOutcomeProtoV1{
		LifeCycleStage:                  string(outcome.LifeCycleStage),
		ObservationTimestampNanoseconds: outcome.ObservationTimestampNanoseconds,
		ChannelDefinitions:              dfns,
		ValidAfterNanoseconds:           validAfterNanoseconds,
		StreamAggregates:                streamAggregates,
	}

	// It's very important that Outcome serialization be deterministic across all nodes!
	// Should be reliable since we don't use maps
	return proto.MarshalOptions{Deterministic: true}.Marshal(pbuf)
}

func validAfterNanosecondsToProtoOutcomeNanoseconds(in map[llotypes.ChannelID]uint64) (out []*LLOChannelIDAndValidAfterNanosecondsProto) {
	if len(in) > 0 {
		out = make([]*LLOChannelIDAndValidAfterNanosecondsProto, 0, len(in))
		for id, v := range in {
			out = append(out, &LLOChannelIDAndValidAfterNanosecondsProto{
				ChannelID:             id,
				ValidAfterNanoseconds: v,
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].ChannelID < out[j].ChannelID
		})
	}
	return
}

func (protoOutcomeCodecV1) Decode(b ocr3types.Outcome) (outcome Outcome, err error) {
	pbuf := &LLOOutcomeProtoV1{}
	err = proto.Unmarshal(b, pbuf)
	if err != nil {
		return Outcome{}, fmt.Errorf("failed to decode outcome: expected protobuf (got: 0x%x); %w", b, err)
	}
	dfns, err := channelDefinitionsFromProtoOutcome(pbuf.ChannelDefinitions)
	if err != nil {
		return Outcome{}, err
	}
	streamAggregates, err := streamAggregatesFromProtoOutcome(pbuf.StreamAggregates)
	if err != nil {
		return Outcome{}, err
	}
	validAfterNanoseconds := validAfterNanosecondsFromProtoOutcomeNanoseconds(pbuf.ValidAfterNanoseconds)
	outcome = Outcome{
		LifeCycleStage:                  llotypes.LifeCycleStage(pbuf.LifeCycleStage),
		ObservationTimestampNanoseconds: pbuf.ObservationTimestampNanoseconds,
		ChannelDefinitions:              dfns,
		ValidAfterNanoseconds:           validAfterNanoseconds,
		StreamAggregates:                streamAggregates,
	}
	return outcome, nil
}

func validAfterNanosecondsFromProtoOutcomeNanoseconds(in []*LLOChannelIDAndValidAfterNanosecondsProto) (out map[llotypes.ChannelID]uint64) {
	if len(in) > 0 {
		out = make(map[llotypes.ChannelID]uint64, len(in))
		for _, v := range in {
			out[v.ChannelID] = v.ValidAfterNanoseconds
		}
	}
	return
}
