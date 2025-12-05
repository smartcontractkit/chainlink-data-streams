package llo

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func VerifyChannelDefinitions(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions) (merr error) {
	if len(channelDefs) > MaxOutcomeChannelDefinitionsLength {
		return fmt.Errorf("too many channels, got: %d/%d", len(channelDefs), MaxOutcomeChannelDefinitionsLength)
	}
	uniqueStreamIDs := make(map[llotypes.StreamID]struct{}, len(channelDefs))
	for channelID, cd := range channelDefs {
		if len(cd.Streams) == 0 {
			merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has no streams", channelID))
			continue
		}
		if len(cd.Streams) > MaxStreamsPerChannel {
			merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has too many streams, got: %d/%d", channelID, len(cd.Streams), MaxStreamsPerChannel))
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
			if err := codec.Verify(cd); err != nil {
				merr = errors.Join(merr, fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, err))
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

type channelDefinitionOptsCache struct {
	mu    sync.RWMutex
	cache map[llotypes.ChannelID]interface{}
}

var _ ChannelDefinitionOptsCache = (*channelDefinitionOptsCache)(nil)

func NewChannelDefinitionOptsCache() ChannelDefinitionOptsCache {
	return &channelDefinitionOptsCache{
		cache: make(map[llotypes.ChannelID]interface{}),
	}
}

func (c *channelDefinitionOptsCache) Set(
	channelID llotypes.ChannelID,
	channelOpts llotypes.ChannelOpts,
	codec ReportCodec,
) error {
	// codec may or may not implement OptsParser interface - that is the codec's choice. 
	// if codec does not then we cannot cache the opts. Codec may do this if they do not have opts. 
	optsParser, ok := codec.(OptsParser)
	if !ok {
		return fmt.Errorf("codec does not implement OptsParser interface")
	}

	parsedOpts, err := optsParser.ParseOpts(channelOpts)
	if err != nil {
		return fmt.Errorf("failed to parse opts for channelID %d: %w", channelID, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[channelID] = parsedOpts
	return nil
}

func (c *channelDefinitionOptsCache) Get(channelID llotypes.ChannelID) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.cache[channelID]
	return val, ok
}

func (c *channelDefinitionOptsCache) Delete(channelID llotypes.ChannelID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, channelID)
}
