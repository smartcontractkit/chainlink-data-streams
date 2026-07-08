package common

import "github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

// Additional limits so we can more effectively bound the size of observations
// NOTE: These are hardcoded because these exact values are relied upon as a
// property of coming to consensus, it's too dangerous to make these
// configurable on a per-node basis. It may be possible to add them to the
// OffchainConfig if they need to be changed dynamically and in a
// backwards-compatible way.
//
// These LLO-protocol limits are shared across all plugin versions.
const (
	// MaxReportCount is the maximum number of reports (and therefore channels)
	// supported. CAREFUL! If we ever accidentally exceed this e.g. through too
	// many channels/streams, the protocol will halt.
	// https://smartcontract-it.atlassian.net/browse/MERC-6468
	MaxReportCount = ocr3types.MaxMaxReportCount

	// Maximum amount of channels that can be removed per round (if more than
	// this need to be removed, they will be removed in batches until
	// everything is up-to-date)
	MaxObservationRemoveChannelIDsLength = 5
	// Maximum amount of channels that can be added/updated per round (if more
	// than this need to be added, they will be added in batches until
	// everything is up-to-date)
	MaxObservationUpdateChannelDefinitionsLength = 5
	// Maximum number of streams that can be observed per round
	MaxObservationStreamValuesLength = 10_000
	// Maximum allowed number of streams per channel
	MaxStreamsPerChannel = 10_000
	// MaxOutcomeChannelDefinitionsLength is the maximum number of channels that
	// can be supported
	MaxOutcomeChannelDefinitionsLength = MaxReportCount
)
