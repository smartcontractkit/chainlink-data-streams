package llo

import (
	"fmt"
	"math"
	"sort"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// NOTE: These codecs make a lot of allocations which will be hard on the
// garbage collector, this can probably be made more efficient

var _ OutcomeCodec = (*protoOutcomeCodecV0)(nil)

type protoOutcomeCodecV0 struct{}

func (protoOutcomeCodecV0) Encode(outcome Outcome) (ocr3types.Outcome, error) {
	dfns := channelDefinitionsToProtoOutcome(outcome.ChannelDefinitions)

	streamAggregates, err := StreamAggregatesToProtoOutcome(outcome.StreamAggregates)
	if err != nil {
		return nil, err
	}

	validAfterSeconds, err := validAfterNanosecondsToProtoOutcomeSeconds(outcome.ValidAfterNanoseconds)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal protobuf; %w", err)
	}

	if outcome.ObservationTimestampNanoseconds > math.MaxInt64 {
		return nil, fmt.Errorf("cannot marshal protobuf; observation timestamp too large: %d", outcome.ObservationTimestampNanoseconds)
	}
	pbuf := &LLOOutcomeProtoV0{
		LifeCycleStage:                  string(outcome.LifeCycleStage),
		ObservationTimestampNanoseconds: int64(outcome.ObservationTimestampNanoseconds),
		ChannelDefinitions:              dfns,
		ValidAfterSeconds:               validAfterSeconds,
		StreamAggregates:                streamAggregates,
	}

	// It's very important that Outcome serialization be deterministic across all nodes!
	// Should be reliable since we don't use maps
	return DeterministicMarshalOptions.Marshal(pbuf)
}

func validAfterNanosecondsToProtoOutcomeSeconds(in map[llotypes.ChannelID]uint64) (out []*LLOChannelIDAndValidAfterSecondsProto, err error) {
	if len(in) > 0 {
		out = make([]*LLOChannelIDAndValidAfterSecondsProto, 0, len(in))
		for id, v := range in {
			seconds := v / 1e9
			if seconds > math.MaxUint32 {
				return nil, fmt.Errorf("valid after seconds too large: %d", seconds)
			}
			out = append(out, &LLOChannelIDAndValidAfterSecondsProto{
				ChannelID:         id,
				ValidAfterSeconds: uint32(seconds),
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].ChannelID < out[j].ChannelID
		})
	}
	return
}

func (protoOutcomeCodecV0) Decode(b ocr3types.Outcome) (outcome Outcome, err error) {
	pbuf := &LLOOutcomeProtoV0{}
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
	validAfterNanoseconds := validAfterNanosecondsFromProtoOutcomeSeconds(pbuf.ValidAfterSeconds)
	if pbuf.ObservationTimestampNanoseconds < 0 {
		return Outcome{}, fmt.Errorf("failed to decode outcome; invalid observation timestamp: %d", pbuf.ObservationTimestampNanoseconds)
	}
	outcome = Outcome{
		LifeCycleStage:                  llotypes.LifeCycleStage(pbuf.LifeCycleStage),
		ObservationTimestampNanoseconds: uint64(pbuf.ObservationTimestampNanoseconds),
		ChannelDefinitions:              dfns,
		ValidAfterNanoseconds:           validAfterNanoseconds,
		StreamAggregates:                streamAggregates,
	}
	return outcome, nil
}

func validAfterNanosecondsFromProtoOutcomeSeconds(in []*LLOChannelIDAndValidAfterSecondsProto) (out map[llotypes.ChannelID]uint64) {
	if len(in) > 0 {
		out = make(map[llotypes.ChannelID]uint64, len(in))
		for _, v := range in {
			out[v.ChannelID] = uint64(v.ValidAfterSeconds) * 1e9
		}
	}
	return
}
