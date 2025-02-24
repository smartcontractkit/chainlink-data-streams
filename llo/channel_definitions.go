package llo

import (
	"context"
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func VerifyChannelDefinitions(ctx context.Context, codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions) (merr error) {
	if len(channelDefs) > MaxOutcomeChannelDefinitionsLength {
		return fmt.Errorf("too many channels, got: %d/%d", len(channelDefs), MaxOutcomeChannelDefinitionsLength)
	}
	uniqueStreamIDs := make(map[llotypes.StreamID]struct{}, len(channelDefs))
	for channelID, cd := range channelDefs {
		if len(cd.Streams) == 0 {
			merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has no streams", channelID))
			continue
		}
		for _, strm := range cd.Streams {
			if strm.Aggregator == 0 {
				merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has stream %d with zero aggregator (this may indicate an uninitialized struct)", channelID, strm.StreamID))
				continue
			}
			uniqueStreamIDs[strm.StreamID] = struct{}{}
		}
		if codec, ok := codecs[cd.ReportFormat]; ok {
			if err := codec.Verify(ctx, cd); err != nil {
				merr = errors.Join(merr, fmt.Errorf("invalid ChannelDefinition with ID %d: %v", channelID, err))
				continue
			}
		}
	}
	if merr != nil {
		return merr
	}
	if len(uniqueStreamIDs) > MaxObservationStreamValuesLength {
		return fmt.Errorf("too many unique stream IDs, got: %d/%d", len(uniqueStreamIDs), MaxObservationStreamValuesLength)
	}
	return nil
}

func subtractChannelDefinitions(minuend llotypes.ChannelDefinitions, subtrahend llotypes.ChannelDefinitions, limit int) llotypes.ChannelDefinitions {
	differenceList := []ChannelDefinitionWithID{}
	for channelID, channelDefinition := range minuend {
		if _, ok := subtrahend[channelID]; !ok {
			differenceList = append(differenceList, ChannelDefinitionWithID{channelDefinition, channelID})
		}
	}

	// Sort so we return deterministic result
	sort.Slice(differenceList, func(i, j int) bool {
		return differenceList[i].ChannelID < differenceList[j].ChannelID
	})

	if len(differenceList) > limit {
		differenceList = differenceList[:limit]
	}

	difference := llotypes.ChannelDefinitions{}
	for _, defWithID := range differenceList {
		difference[defWithID.ChannelID] = defWithID.ChannelDefinition
	}

	return difference
}
