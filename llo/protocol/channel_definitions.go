package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func VerifyChannelDefinitions(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions) (merr error) {
	if len(channelDefs) > MaxOutcomeChannelDefinitionsLength {
		return fmt.Errorf("too many channels, got: %d/%d", len(channelDefs), MaxOutcomeChannelDefinitionsLength)
	}

	// Verify in ascending channel ID order so that the errors a rejected
	// definitions file produces are the same on every oracle.
	channelIDs := make([]llotypes.ChannelID, 0, len(channelDefs))
	for channelID := range channelDefs {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })

	uniqueStreamIDs := make(map[llotypes.StreamID]struct{}, len(channelDefs))
	// Owners of every stream ID that will hold an aggregate: observed streams
	// come from the definitions, calculated streams from the expressions their
	// channel's opts declare. Both are collected here so that the uniqueness
	// check below can run once over the whole set.
	observedBy := make(map[llotypes.StreamID]llotypes.ChannelID, len(channelDefs))
	calculatedBy := make(map[llotypes.StreamID]llotypes.ChannelID)
	// Owners of every feed ID the definitions publish under. Two channels
	// sharing one publish conflicting reports for the same on-chain feed, so the
	// second is rejected.
	feedIDBy := make(map[[32]byte]llotypes.ChannelID)

	for _, channelID := range channelIDs {
		cd := channelDefs[channelID]
		if cd.Tombstone {
			continue
		}

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
			// Calculated streams are appended to the definition by expression
			// evaluation, so they are recorded from the opts that declare them
			// rather than from here: a definition inherited from a previous
			// outcome already carries its channel's own calculated streams, and
			// treating those as observed would report every such channel as
			// colliding with itself.
			if strm.Aggregator != llotypes.AggregatorCalculated {
				if _, ok := observedBy[strm.StreamID]; !ok {
					observedBy[strm.StreamID] = channelID
				}
			}
		}
		if cd.ReportFormat == llotypes.ReportFormatEVMABIEncodeUnpackedExpr {
			ids, err := expressionStreamIDs(cd)
			if err != nil {
				merr = errors.Join(merr, fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, err))
			}
			for _, streamID := range ids {
				if owner, ok := calculatedBy[streamID]; ok {
					merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d declares calculated stream %d already declared by channel %d", channelID, streamID, owner))
					continue
				}
				calculatedBy[streamID] = channelID
			}
		}
		var verifyErr error
		if codec, ok := codecs[cd.ReportFormat]; ok {
			verifyErr = codec.Verify(cd)
			if verifyErr != nil {
				merr = errors.Join(merr, fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, verifyErr))
			}
		}
		if feedIDer, ok := codecs[cd.ReportFormat].(FeedIDer); ok && verifyErr == nil {
			feedID, hasFeedID, err := feedIDer.FeedID(cd)
			switch {
			case err != nil:
				merr = errors.Join(merr, fmt.Errorf("invalid ChannelDefinition with ID %d: failed to resolve feed ID: %w", channelID, err))
			case !hasFeedID:
			default:
				if owner, ok := feedIDBy[feedID]; ok {
					merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has feed ID 0x%x already used by channel %d", channelID, feedID, owner))
				} else {
					feedIDBy[feedID] = channelID
				}
			}
		}
		if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			if err := ValidateHistoryBackfillAgainstDefinitions(cd, channelDefs, 0); err != nil {
				merr = errors.Join(merr, fmt.Errorf("invalid history backfill channel %d: %w", channelID, err))
			}
		}
		if verifyErr != nil {
			continue
		}
	}

	// A calculated stream that shares its ID with an observed stream would write
	// over an aggregate that is not its own. Evaluation refuses to do that, so
	// such a channel can never report, and the ID it clashes with is only
	// visible across the whole definition set -- which is why it is checked
	// here, once, rather than per channel. Only observed streams need a second
	// pass: calculated collisions are caught as they are collected above.
	for _, streamID := range sortedStreamIDs(calculatedBy) {
		if owner, ok := observedBy[streamID]; ok {
			merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d declares calculated stream %d, which channel %d observes", calculatedBy[streamID], streamID, owner))
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

func sortedStreamIDs(m map[llotypes.StreamID]llotypes.ChannelID) []llotypes.StreamID {
	ids := make([]llotypes.StreamID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// expressionStreamIDs decodes the calculated stream IDs an expression channel's
// opts declare. It duplicates the shape of the calculated package's opts rather
// than calling into it because that package depends on this one; only the field
// this check needs is decoded, so the shapes cannot drift in a way that matters.
//
// Returns the IDs decoded so far along with the first error, so a definition
// with a bad entry still contributes its good ones to the uniqueness check.
func expressionStreamIDs(cd llotypes.ChannelDefinition) ([]llotypes.StreamID, error) {
	var opts struct {
		ABI []struct {
			ExpressionStreamID llotypes.StreamID `json:"expressionStreamID"`
		} `json:"abi"`
	}
	if err := json.Unmarshal(cd.Opts, &opts); err != nil {
		return nil, fmt.Errorf("failed to decode calculated stream opts: %w", err)
	}
	if len(opts.ABI) == 0 {
		return nil, errors.New("no expressions found in channel definition")
	}
	ids := make([]llotypes.StreamID, 0, len(opts.ABI))
	for i, abi := range opts.ABI {
		if abi.ExpressionStreamID == 0 {
			return ids, fmt.Errorf("expression stream ID is 0, abi index: %d", i)
		}
		ids = append(ids, abi.ExpressionStreamID)
	}
	return ids, nil
}

func SubtractChannelDefinitions(minuend llotypes.ChannelDefinitions, subtrahend llotypes.ChannelDefinitions, limit int) llotypes.ChannelDefinitions {
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
