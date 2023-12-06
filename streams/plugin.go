package streams

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	commontypes "github.com/smartcontractkit/chainlink-common/pkg/types"

	chainselectors "github.com/smartcontractkit/chain-selectors"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr3types "github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// TODO: Split out this file

// Notes:
//
// This is a sketch, there are many improvements to be made for this to be
// production-grade, secure code.
//
// We use JSON for serialization/deserialization. We rely on the fact that
// golang's json package serializes maps deterministically. Protobufs would
// likely be a more performant & efficient choice.

// Additional limits so we can more effectively bound the size of observations
const (
	MAX_OBSERVATION_REMOVE_CHANNEL_IDS_LENGTH      = 5
	MAX_OBSERVATION_ADD_CHANNEL_DEFINITIONS_LENGTH = 5
	MAX_OBSERVATION_STREAM_VALUES_LENGTH           = 1_000
)

const MAX_OUTCOME_CHANNEL_DEFINITIONS_LENGTH = 500

// Values for a set of streams, e.g. "eth-usd", "link-usd", and "eur-chf"
// TODO: generalize from *big.Int to anything
// TODO: Consider renaming to StreamDataPoints?
// FIXME: error vs. valid
type StreamValues map[StreamID]ObsResult[*big.Int]

type DataSource interface {
	// For each known streamID, Observe should return a non-nil entry in
	// StreamValues. Observe should ignore unknown streamIDs.
	Observe(ctx context.Context, streamIDs map[StreamID]struct{}) (StreamValues, error)
}

// type LifeCycleStage string

// Protocol instances start in either the staging or production stage. They
// may later be retired and "hand over" their work to another protocol instance
// that will move from the staging to the production stage.
const (
	LifeCycleStageStaging    commontypes.StreamsLifeCycleStage = "staging"
	LifeCycleStageProduction commontypes.StreamsLifeCycleStage = "production"
	LifeCycleStageRetired    commontypes.StreamsLifeCycleStage = "retired"
)

type RetirementReport struct {
	// Carries validity time stamps between protocol instances to ensure there
	// are no gaps
	ValidAfterSeconds map[ChannelID]uint32
}

type ShouldRetireCache interface { // reads asynchronously from onchain ConfigurationStore
	// Should the protocol instance retire according to the configuration
	// contract?
	// See: https://github.com/smartcontractkit/mercury-v1-sketch/blob/main/onchain/src/ConfigurationStore.sol#L18
	ShouldRetire() (bool, error)
}

// The predecessor protocol instance stores its attested retirement report in
// this cache (locally, offchain), so it can be fetched by the successor
// protocol instance.
// TODO: This ought to be DB-persisted
//
// PredecessorRetirementReportCache is populated by the old protocol instance
// writing to it and the new protocol instance reading from it.
//
// The sketch envisions it being implemented as a single object that is shared
// between different protocol instances.
type PredecessorRetirementReportCache interface {
	AttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest) ([]byte, error)
	CheckAttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest, attestedRetirementReport []byte) (RetirementReport, error)
}

const (
	ReportFormatEVM  commontypes.StreamsReportFormat = "evm"
	ReportFormatJSON commontypes.StreamsReportFormat = "json"
	// Solana, CosmWasm, kalechain, etc... all go here
)

// QUESTION: Do we also want to include an (optional) designated verifier
// address, i.e. the only address allowed to verify reports from this channel
type ChannelDefinition struct {
	ReportFormat commontypes.StreamsReportFormat
	// Specifies the chain on which this channel can be verified. Currently uses
	// CCIP chain selectors.
	ChainSelector uint64
	// We assume that StreamIDs is always non-empty and that the 0-th stream
	// contains the verification price in LINK and the 1-st stream contains the
	// verification price in the native coin.
	StreamIDs []StreamID
}

type ChannelDefinitionWithID struct {
	ChannelDefinition
	ChannelID ChannelID
}

type ChannelDefinitions map[ChannelID]ChannelDefinition

type ChannelHash [32]byte

func MakeChannelHash(cd ChannelDefinitionWithID) ChannelHash {
	h := sha256.New()
	h.Write(cd.ChannelID[:])
	binary.Write(h, binary.BigEndian, uint32(len(cd.ReportFormat)))
	h.Write([]byte(cd.ReportFormat))
	binary.Write(h, binary.BigEndian, cd.ChainSelector)
	binary.Write(h, binary.BigEndian, uint32(len(cd.StreamIDs)))
	for _, stream := range cd.StreamIDs {
		binary.Write(h, binary.BigEndian, uint32(len(stream)))
		h.Write([]byte(stream))
	}
	var result [32]byte
	h.Sum(result[:0])
	return result
}

// An ReportingPlugin allows plugging custom logic into the OCR3 protocol. The OCR
// protocol handles cryptography, networking, ensuring that a sufficient number
// of nodes is in agreement about any report, transmitting the report to the
// contract, etc... The ReportingPlugin handles application-specific logic. To do so,
// the ReportingPlugin defines a number of callbacks that are called by the OCR
// protocol logic at certain points in the protocol's execution flow. The report
// generated by the ReportingPlugin must be in a format understood by contract that
// the reports are transmitted to.
//
// We assume that each correct node participating in the protocol instance will
// be running the same ReportingPlugin implementation. However, not all nodes may be
// correct; up to f nodes be faulty in arbitrary ways (aka byzantine faults).
// For example, faulty nodes could be down, have intermittent connectivity
// issues, send garbage messages, or be controlled by an adversary.
//
// For a protocol round where everything is working correctly, followers will
// call Observation, Outcome, and Reports. For each report,
// ShouldAcceptAttestedReport will be called as well. If
// ShouldAcceptAttestedReport returns true, ShouldTransmitAcceptedReport will
// be called. However, an ReportingPlugin must also correctly handle the case where
// faults occur.
//
// In particular, an ReportingPlugin must deal with cases where:
//
// - only a subset of the functions on the ReportingPlugin are invoked for a given
// round
//
// - an arbitrary number of seqnrs has been skipped between invocations of the
// ReportingPlugin
//
// - the observation returned by Observation is not included in the list of
// AttributedObservations passed to Report
//
// - a query or observation is malformed. (For defense in depth, it is also
// recommended that malformed outcomes are handled gracefully.)
//
// - instances of the ReportingPlugin run by different oracles have different call
// traces. E.g., the ReportingPlugin's Observation function may have been invoked on
// node A, but not on node B.
//
// All functions on an ReportingPlugin should be thread-safe.
//
// All functions that take a context as their first argument may still do cheap
// computations after the context expires, but should stop any blocking
// interactions with outside services (APIs, database, ...) and return as
// quickly as possible. (Rough rule of thumb: any such computation should not
// take longer than a few ms.) A blocking function may block execution of the
// entire protocol instance on its node!
//
// For a given OCR protocol instance, there can be many (consecutive) instances
// of an ReportingPlugin, e.g. due to software restarts. If you need ReportingPlugin state
// to survive across restarts, you should store it in the Outcome or persist it.
// An ReportingPlugin instance will only ever serve a single protocol instance.
var _ ocr3types.ReportingPluginFactory[commontypes.StreamsReportInfo] = &PluginFactory{}

func NewPluginFactory(prrc PredecessorRetirementReportCache, src ShouldRetireCache, cdc ChannelDefinitionCache, ds DataSource, lggr logger.Logger, codecs map[commontypes.StreamsReportFormat]ReportCodec) *PluginFactory {
	return &PluginFactory{
		prrc, src, cdc, ds, lggr, codecs,
	}
}

type PluginFactory struct {
	PredecessorRetirementReportCache PredecessorRetirementReportCache
	ShouldRetireCache                ShouldRetireCache
	ChannelDefinitionCache           ChannelDefinitionCache
	DataSource                       DataSource
	Logger                           logger.Logger
	Codecs                           map[commontypes.StreamsReportFormat]ReportCodec
}

func (f *PluginFactory) NewReportingPlugin(cfg ocr3types.ReportingPluginConfig) (ocr3types.ReportingPlugin[commontypes.StreamsReportInfo], ocr3types.ReportingPluginInfo, error) {
	var predecessorConfigDigest *types.ConfigDigest
	if len(cfg.OffchainConfig) == 0 {
		predecessorConfigDigest = nil
		// TODO: Switch to format that can support additional fields e.g. JSON or protobuf
		// We use the offchainconfig of the plugin to tell the plugin the configdigest of its predecessor protocol instance
		// NOTE: Set here: https://github.com/smartcontractkit/mercury-v1-sketch/blob/f52c0f823788f86c1aeaa9ba1eee32a85b981535/onchain/src/ConfigurationStore.sol#L13
		//
		// QUESTION: Previously we stored ExpiryWindow and BaseUSDFeeCents in offchain
		// config, but those might be channel specific so need to move to
		// channel definition
	} else if len(cfg.OffchainConfig) == len(types.ConfigDigest{}) {
		var pcd types.ConfigDigest
		copy(pcd[:], cfg.OffchainConfig)
		predecessorConfigDigest = &pcd
	} else {
		return nil, ocr3types.ReportingPluginInfo{}, fmt.Errorf("invalid OffchainConfig")
	}

	return &StreamsPlugin{
			predecessorConfigDigest,
			cfg.ConfigDigest,
			f.PredecessorRetirementReportCache,
			f.ShouldRetireCache,
			f.ChannelDefinitionCache,
			f.DataSource,
			f.Logger,
			cfg.F,
			f.Codecs,
		}, ocr3types.ReportingPluginInfo{
			Name: "Streams",
			Limits: ocr3types.ReportingPluginLimits{
				MaxQueryLength:       0,
				MaxObservationLength: ocr3types.MaxMaxObservationLength, // TODO: use tighter bound
				MaxOutcomeLength:     ocr3types.MaxMaxOutcomeLength,     // TODO: use tighter bound
				MaxReportLength:      ocr3types.MaxMaxReportLength,      // TODO: use tighter bound
				MaxReportCount:       ocr3types.MaxMaxReportCount,       // TODO: use tighter bound
			},
		}, nil
}

var _ ocr3types.ReportingPlugin[commontypes.StreamsReportInfo] = &StreamsPlugin{}

type ReportCodec interface {
	Encode(Report) ([]byte, error)
}

type StreamsPlugin struct {
	PredecessorConfigDigest          *types.ConfigDigest
	ConfigDigest                     types.ConfigDigest
	PredecessorRetirementReportCache PredecessorRetirementReportCache
	ShouldRetireCache                ShouldRetireCache
	ChannelDefinitionCache           ChannelDefinitionCache
	DataSource                       DataSource
	Logger                           logger.Logger
	F                                int
	Codecs                           map[commontypes.StreamsReportFormat]ReportCodec
}

// Query creates a Query that is sent from the leader to all follower nodes
// as part of the request for an observation. Be careful! A malicious leader
// could equivocate (i.e. send different queries to different followers.)
// Many applications will likely be better off always using an empty query
// if the oracles don't need to coordinate on what to observe (e.g. in case
// of a price feed) or the underlying data source offers an (eventually)
// consistent view to different oracles (e.g. in case of observing a
// blockchain).
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *StreamsPlugin) Query(ctx context.Context, outctx ocr3types.OutcomeContext) (types.Query, error) {
	return nil, nil
}

type Observation struct {
	// Attested (i.e. signed by f+1 oracles) retirement report from predecessor
	// protocol instance
	AttestedPredecessorRetirement []byte
	// Should this protocol instance be retired?
	ShouldRetire bool
	// Timestamp from when observation is made
	UnixTimestampNanoseconds int64
	// Votes to remove/add channels. Subject to MAX_OBSERVATION_*_LENGTH limits
	RemoveChannelIDs      map[ChannelID]struct{}
	AddChannelDefinitions ChannelDefinitions
	// Observed (numeric) stream values. Subject to
	// MAX_OBSERVATION_STREAM_VALUES_LENGTH limit
	StreamValues StreamValues
}

// Observation gets an observation from the underlying data source. Returns
// a value or an error.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *StreamsPlugin) Observation(ctx context.Context, outctx ocr3types.OutcomeContext, query types.Query) (types.Observation, error) {
	// send empty observation in initial round
	// NOTE: First sequence number is always 1
	if outctx.SeqNr < 1 {
		return types.Observation{}, fmt.Errorf("got invalid seqnr=%d, must be >=1", outctx.SeqNr)
	} else if outctx.SeqNr == 1 {
		return types.Observation{}, nil
	}

	// QUESTION: is there a way to have this captured in EAs so we get something
	// closer to the source?
	nowNanoseconds := time.Now().UnixNano()

	var previousOutcome Outcome
	if err := json.Unmarshal(outctx.PreviousOutcome, &previousOutcome); err != nil {
		return nil, fmt.Errorf("error unmarshalling previous outcome: %w", err)
	}

	var attestedRetirementReport []byte
	// Only try to fetch this from the cache if this instance if configured
	// with a predecessor and we're still in the staging stage.
	if p.PredecessorConfigDigest != nil && previousOutcome.LifeCycleStage == LifeCycleStageStaging {
		var err error
		attestedRetirementReport, err = p.PredecessorRetirementReportCache.AttestedRetirementReport(*p.PredecessorConfigDigest)
		if err != nil {
			return nil, fmt.Errorf("error fetching attested retirement report from cache: %w", err)
		}
	}

	shouldRetire, err := p.ShouldRetireCache.ShouldRetire()
	if err != nil {
		return nil, fmt.Errorf("error fetching shouldRetire from cache: %w", err)
	}

	// vote to remove channel ids if they're in the previous outcome
	// ChannelDefinitions or ValidAfterSeconds
	removeChannelIDs := map[ChannelID]struct{}{}
	// vote to add channel definitions that aren't present in the previous
	// outcome ChannelDefinitions
	var addChannelDefinitions ChannelDefinitions
	{
		expectedChannelDefs := p.ChannelDefinitionCache.Definitions()

		removeChannelDefinitions := subtractChannelDefinitions(previousOutcome.ChannelDefinitions, expectedChannelDefs, MAX_OBSERVATION_REMOVE_CHANNEL_IDS_LENGTH)
		for channelID := range removeChannelDefinitions {
			removeChannelIDs[channelID] = struct{}{}
		}

		for channelID := range previousOutcome.ValidAfterSeconds {
			if len(removeChannelIDs) >= MAX_OBSERVATION_REMOVE_CHANNEL_IDS_LENGTH {
				break
			}
			if _, ok := expectedChannelDefs[channelID]; !ok {
				removeChannelIDs[channelID] = struct{}{}
			}
		}

		addChannelDefinitions = subtractChannelDefinitions(expectedChannelDefs, previousOutcome.ChannelDefinitions, MAX_OBSERVATION_ADD_CHANNEL_DEFINITIONS_LENGTH)
	}

	var streamValues StreamValues
	{
		streams := map[StreamID]struct{}{}
		for _, channelDefinition := range previousOutcome.ChannelDefinitions {
			for _, streamID := range channelDefinition.StreamIDs {
				streams[streamID] = struct{}{}
			}
		}

		var err error
		// TODO: Should probably be a slice, not map?
		streamValues, err = p.DataSource.Observe(ctx, streams)
		if err != nil {
			return nil, fmt.Errorf("DataSource.Observe error: %w", err)
		}
	}

	var rawObservation []byte
	{
		var err error
		rawObservation, err = json.Marshal(Observation{
			attestedRetirementReport,
			shouldRetire,
			nowNanoseconds,
			removeChannelIDs,
			addChannelDefinitions,
			streamValues,
		})
		if err != nil {
			return nil, fmt.Errorf("json.Marshal error: %w", err)
		}
	}

	return rawObservation, nil
}

// Should return an error if an observation isn't well-formed.
// Non-well-formed  observations will be discarded by the protocol. This is
// called for each observation, don't do anything slow in here.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *StreamsPlugin) ValidateObservation(outctx ocr3types.OutcomeContext, query types.Query, ao types.AttributedObservation) error {
	if outctx.SeqNr <= 1 {
		if len(ao.Observation) != 0 {
			return fmt.Errorf("Observation is not empty")
		}
	}

	var observation Observation
	err := json.Unmarshal(ao.Observation, &observation)
	if err != nil {
		return fmt.Errorf("Observation is invalid json: %w", err)
	}

	if p.PredecessorConfigDigest == nil && len(observation.AttestedPredecessorRetirement) != 0 {
		return fmt.Errorf("AttestedPredecessorRetirement is not empty even though this instance has no predecessor")
	}

	if len(observation.AddChannelDefinitions) > MAX_OBSERVATION_ADD_CHANNEL_DEFINITIONS_LENGTH {
		return fmt.Errorf("AddChannelDefinitions is too long: %v vs %v", len(observation.AddChannelDefinitions), MAX_OBSERVATION_ADD_CHANNEL_DEFINITIONS_LENGTH)
	}

	if len(observation.RemoveChannelIDs) > MAX_OBSERVATION_REMOVE_CHANNEL_IDS_LENGTH {
		return fmt.Errorf("RemoveChannelIDs is too long: %v vs %v", len(observation.RemoveChannelIDs), MAX_OBSERVATION_REMOVE_CHANNEL_IDS_LENGTH)
	}

	if len(observation.StreamValues) > MAX_OBSERVATION_STREAM_VALUES_LENGTH {
		return fmt.Errorf("StreamValues is too long: %v vs %v", len(observation.StreamValues), MAX_OBSERVATION_STREAM_VALUES_LENGTH)
	}

	for streamID, obsResult := range observation.StreamValues {
		if obsResult.Val == nil {
			// FIXME: shouldn't be possible if its valid
			return fmt.Errorf("stream with id %q carries nil value", streamID)
		}
	}

	return nil
}

type Outcome struct {
	// LifeCycleStage the protocol is in
	LifeCycleStage commontypes.StreamsLifeCycleStage
	// ObservationsTimestampNanoseconds is the median timestamp from the
	// latest set of observations
	ObservationsTimestampNanoseconds int64
	// ChannelDefinitions defines the set & structure of channels for which we
	// generate reports
	ChannelDefinitions ChannelDefinitions
	// Latest ValidAfterSeconds value for each channel, reports for each channel
	// span from ValidAfterSeconds to ObservationTimestampSeconds
	ValidAfterSeconds map[ChannelID]uint32
	// StreamMedians is the median observed value for each stream
	// QUESTION: Can we use arbitrary types here to allow for other types or
	// consensus methods?
	StreamMedians map[StreamID]*big.Int
}

// The Outcome's ObservationsTimestamp rounded down to seconds precision
func (out *Outcome) ObservationsTimestampSeconds() (uint32, error) {
	result := time.Unix(0, out.ObservationsTimestampNanoseconds).Unix()
	if int64(uint32(result)) != result {
		return 0, fmt.Errorf("timestamp doesn't fit into uint32: %v", result)
	}
	return uint32(result), nil
}

// Indicates whether a report can be generated for the given channel.
// TODO: Return error indicating why it isn't reportable
func (out *Outcome) IsReportable(channelID ChannelID) bool {
	if out.LifeCycleStage == LifeCycleStageRetired {
		return false
	}

	observationsTimestampSeconds, err := out.ObservationsTimestampSeconds()
	if err != nil {
		return false
	}

	channelDefinition, ok := out.ChannelDefinitions[channelID]
	if !ok {
		return false
	}

	if _, err := chainselectors.ChainIdFromSelector(channelDefinition.ChainSelector); err != nil {
		return false
	}

	for _, streamID := range channelDefinition.StreamIDs {
		if out.StreamMedians[streamID] == nil {
			return false
		}
	}

	if _, ok := out.ValidAfterSeconds[channelID]; !ok {
		// No validAfterSeconds entry yet, this must be a new channel.
		// validAfterSeconds will be populated in Outcome() so the channel
		// becomes reportable in later protocol rounds.
		return false
	}

	if out.ValidAfterSeconds[channelID] >= observationsTimestampSeconds {
		return false
	}

	return true
}

// List of reportable channels (according to IsReportable), sorted according
// to a canonical ordering
func (out *Outcome) ReportableChannels() []ChannelID {
	result := []ChannelID{}

	for channelID := range out.ChannelDefinitions {
		if !out.IsReportable(channelID) {
			continue
		}
		result = append(result, channelID)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Less(result[j])
	})

	return result
}

// Generates an outcome for a seqNr, typically based on the previous
// outcome, the current query, and the current set of attributed
// observations.
//
// This function should be pure. Don't do anything slow in here.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *StreamsPlugin) Outcome(outctx ocr3types.OutcomeContext, query types.Query, aos []types.AttributedObservation) (ocr3types.Outcome, error) {
	if outctx.SeqNr <= 1 {
		// Initial Outcome
		var lifeCycleStage commontypes.StreamsLifeCycleStage
		if p.PredecessorConfigDigest == nil {
			// Start straight in production if we have no predecessor
			lifeCycleStage = LifeCycleStageProduction
		} else {
			lifeCycleStage = LifeCycleStageStaging
		}
		outcome := Outcome{
			lifeCycleStage,
			0,
			nil,
			nil,
			nil,
		}
		return json.Marshal(outcome)
	}

	/////////////////////////////////
	// Decode previousOutcome
	/////////////////////////////////
	var previousOutcome Outcome
	if err := json.Unmarshal(outctx.PreviousOutcome, &previousOutcome); err != nil {
		return nil, fmt.Errorf("error unmarshalling previous outcome: %v", err)
	}

	/////////////////////////////////
	// Decode observations
	/////////////////////////////////

	// a single valid retirement report is enough
	var validPredecessorRetirementReport *RetirementReport

	shouldRetireVotes := 0

	timestampsNanoseconds := []int64{}

	removeChannelVotesByID := map[ChannelID]int{}

	// for each channelId count number of votes that mention it and count number of votes that include it.
	addChannelVotesByHash := map[ChannelHash]int{}
	addChannelDefinitionsByHash := map[ChannelHash]ChannelDefinitionWithID{}

	streamObservations := map[StreamID][]*big.Int{}

	for _, ao := range aos {
		observation := Observation{}
		// TODO: Use protobufs
		if err := json.Unmarshal(ao.Observation, &observation); err != nil {
			p.Logger.Warnw("ignoring invalid observation", "oracleID", ao.Observer, "error", err)
			continue
		}

		if len(observation.AttestedPredecessorRetirement) != 0 && validPredecessorRetirementReport == nil {
			pcd := *p.PredecessorConfigDigest
			retirementReport, err := p.PredecessorRetirementReportCache.CheckAttestedRetirementReport(pcd, observation.AttestedPredecessorRetirement)
			if err != nil {
				p.Logger.Warnw("ignoring observation with invalid attested predecessor retirement", "oracleID", ao.Observer, "error", err, "predecessorConfigDigest", pcd)
				continue
			}
			validPredecessorRetirementReport = &retirementReport
		}

		if observation.ShouldRetire {
			shouldRetireVotes++
		}

		timestampsNanoseconds = append(timestampsNanoseconds, observation.UnixTimestampNanoseconds)

		for channelID := range observation.RemoveChannelIDs {
			removeChannelVotesByID[channelID]++
		}

		for channelID, channelDefinition := range observation.AddChannelDefinitions {
			defWithID := ChannelDefinitionWithID{channelDefinition, channelID}
			channelHash := MakeChannelHash(defWithID)
			addChannelVotesByHash[channelHash]++
			addChannelDefinitionsByHash[channelHash] = defWithID
		}

		for id, obsResult := range observation.StreamValues {
			if obsResult.Err == nil {
				streamObservations[id] = append(streamObservations[id], obsResult.Val)
			}
		}
	}

	var outcome Outcome

	/////////////////////////////////
	// outcome.LifeCycleStage
	/////////////////////////////////
	if previousOutcome.LifeCycleStage == LifeCycleStageStaging && validPredecessorRetirementReport != nil {
		// Promote this protocol instance to the production stage! 🚀

		// override ValidAfterSeconds with the value from the retirement report
		// so that we have no gaps in the validity time range.
		outcome.ValidAfterSeconds = validPredecessorRetirementReport.ValidAfterSeconds
		outcome.LifeCycleStage = LifeCycleStageProduction
	} else {
		outcome.LifeCycleStage = previousOutcome.LifeCycleStage
	}

	if outcome.LifeCycleStage == LifeCycleStageProduction && shouldRetireVotes > p.F {
		outcome.LifeCycleStage = LifeCycleStageRetired
	}

	/////////////////////////////////
	// outcome.ObservationsTimestampNanoseconds
	/////////////////////////////////
	sort.Slice(timestampsNanoseconds, func(i, j int) bool { return timestampsNanoseconds[i] < timestampsNanoseconds[j] })
	outcome.ObservationsTimestampNanoseconds = timestampsNanoseconds[len(timestampsNanoseconds)/2]

	/////////////////////////////////
	// outcome.ChannelDefinitions
	/////////////////////////////////
	outcome.ChannelDefinitions = previousOutcome.ChannelDefinitions
	if outcome.ChannelDefinitions == nil {
		outcome.ChannelDefinitions = ChannelDefinitions{}
	}

	// if retired, stop updating channel definitions
	if outcome.LifeCycleStage == LifeCycleStageRetired {
		removeChannelVotesByID, addChannelDefinitionsByHash = nil, nil
	}

	var removedChannelIDs []ChannelID
	for channelID, voteCount := range removeChannelVotesByID {
		if voteCount <= p.F {
			continue
		}
		removedChannelIDs = append(removedChannelIDs, channelID)
		delete(outcome.ChannelDefinitions, channelID)
	}

	for channelHash, defWithID := range addChannelDefinitionsByHash {
		voteCount := addChannelVotesByHash[channelHash]
		if voteCount <= p.F {
			continue
		}
		if conflictDef, exists := outcome.ChannelDefinitions[defWithID.ChannelID]; exists {
			p.Logger.Warn("More than f nodes vote to add a channel, but a channel with the same id already exists",
				"existingChannelDefinition", conflictDef,
				"addChannelDefinition", defWithID,
			)
			continue
		}
		if len(outcome.ChannelDefinitions) > MAX_OUTCOME_CHANNEL_DEFINITIONS_LENGTH {
			p.Logger.Warn("Cannot add channel, outcome already contains maximum number of channels",
				"maxOutcomeChannelDefinitionsLength", MAX_OUTCOME_CHANNEL_DEFINITIONS_LENGTH,
				"addChannelDefinition", defWithID,
			)
			continue
		}
		outcome.ChannelDefinitions[defWithID.ChannelID] = defWithID.ChannelDefinition
	}

	/////////////////////////////////
	// outcome.ValidAfterSeconds
	/////////////////////////////////

	if outcome.ValidAfterSeconds != nil {
		// earlier code already populated ValidAfterSeconds during promotion to
		// production, do nothing
	} else {
		previousObservationsTimestampSeconds, err := previousOutcome.ObservationsTimestampSeconds()
		if err != nil {
			return nil, fmt.Errorf("error getting previous outcome's observations timestamp: %v", err)
		}

		outcome.ValidAfterSeconds = map[ChannelID]uint32{}
		for channelID, previousValidAfterSeconds := range previousOutcome.ValidAfterSeconds {
			if previousOutcome.IsReportable(channelID) {
				// was reported based on previous outcome
				outcome.ValidAfterSeconds[channelID] = previousObservationsTimestampSeconds
			} else {
				// was skipped based on previous outcome
				outcome.ValidAfterSeconds[channelID] = previousValidAfterSeconds
			}
		}
	}

	observationsTimestampSeconds, err := outcome.ObservationsTimestampSeconds()
	if err != nil {
		return nil, fmt.Errorf("error getting outcome's observations timestamp: %w", err)
	}

	for channelID := range outcome.ChannelDefinitions {
		if _, ok := outcome.ValidAfterSeconds[channelID]; !ok {
			// new channel, set validAfterSeconds to observations timestamp
			outcome.ValidAfterSeconds[channelID] = observationsTimestampSeconds
		}
	}

	// One might think that we should simply delete any channel from
	// ValidAfterSeconds that is not mentioned in the ChannelDefinitions. This
	// could, however, lead to gaps being created if this protocol instance is
	// promoted from staging to production while we're still "ramping up" the
	// full set of channels. We do the "safe" thing (i.e. minimizing occurrence
	// of gaps) here and only remove channels if there has been an explicit vote
	// to remove them.
	for _, channelID := range removedChannelIDs {
		delete(outcome.ValidAfterSeconds, channelID)
	}

	/////////////////////////////////
	// outcome.StreamMedians
	/////////////////////////////////
	outcome.StreamMedians = map[StreamID]*big.Int{}
	for streamID, observations := range streamObservations {
		sort.Slice(observations, func(i, j int) bool { return observations[i].Cmp(observations[j]) < 0 })
		if len(observations) <= p.F {
			continue
		}
		// We use a "rank-k" median here, instead one could average in case of
		// an even number of observations.
		outcome.StreamMedians[streamID] = observations[len(observations)/2]
	}

	return json.Marshal(outcome)
}

type Report struct {
	ConfigDigest types.ConfigDigest
	// Chain the report is destined for
	ChainSelector uint64
	// OCR sequence number of this report
	SeqNr uint64
	// Channel that is being reported on
	ChannelID ChannelID
	// Report is valid for ValidAfterSeconds < block.time <= ValidUntilSeconds
	ValidAfterSeconds uint32
	ValidUntilSeconds uint32
	// Here we only encode big.Ints, but in principle there's nothing stopping
	// us from also supporting non-numeric data or smaller values etc...
	Values []*big.Int
	// The contract onchain will only validate non-specimen reports. A staging
	// protocol instance will generate specimen reports so we can validate it
	// works properly without any risk of misreports landing on chain.
	Specimen bool
}

// func getEVMReportTypes() abi.Arguments {
//     mustNewType := func(t string) abi.Type {
//         result, err := abi.NewType(t, "", []abi.ArgumentMarshaling{})
//         if err != nil {
//             panic(fmt.Sprintf("Unexpected error during abi.NewType: %s", err))
//         }
//         return result
//     }
//     return abi.Arguments([]abi.Argument{
//         {Name: "configDigest", Type: mustNewType("bytes32")},
//         {Name: "chainId", Type: mustNewType("uint64")},
//         // could also include address of verifier to make things more specific.
//         // downside is increased data size.
//         // for now we assume that a channelId will only be registered on a single
//         // verifier per chain.
//         {Name: "seqNr", Type: mustNewType("uint64")},
//         {Name: "channelId", Type: mustNewType("bytes32")},
//         {Name: "validAfterSeconds", Type: mustNewType("uint32")},
//         {Name: "validUntilSeconds", Type: mustNewType("uint32")},
//         {Name: "values", Type: mustNewType("int192[]")},
//         {Name: "specimen", Type: mustNewType("bool")},
//     })
// }

func (p *StreamsPlugin) encodeReport(r Report, format commontypes.StreamsReportFormat) (types.Report, error) {
	codec, exists := p.Codecs[format]
	if !exists {
		return nil, fmt.Errorf("codec for ReportFormat=%s missing", format)
	}
	return codec.Encode(r)
}

// Generates a (possibly empty) list of reports from an outcome. Each report
// will be signed and possibly be transmitted to the contract. (Depending on
// ShouldAcceptAttestedReport & ShouldTransmitAcceptedReport)
//
// This function should be pure. Don't do anything slow in here.
//
// This is likely to change in the future. It will likely be returning a
// list of report batches, where each batch goes into its own Merkle tree.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *StreamsPlugin) Reports(seqNr uint64, rawOutcome ocr3types.Outcome) ([]ocr3types.ReportWithInfo[commontypes.StreamsReportInfo], error) {
	if seqNr <= 1 {
		// no reports for initial round
		return nil, nil
	}

	var outcome Outcome
	if err := json.Unmarshal(rawOutcome, &outcome); err != nil {
		return nil, fmt.Errorf("error unmarshalling outcome: %w", err)
	}

	observationsTimestampSeconds, err := outcome.ObservationsTimestampSeconds()
	if err != nil {
		return nil, fmt.Errorf("error getting observations timestamp: %w", err)
	}

	rwis := []ocr3types.ReportWithInfo[commontypes.StreamsReportInfo]{}

	if outcome.LifeCycleStage == LifeCycleStageRetired {
		// if we're retired, emit special retirement report to transfer
		// ValidAfterSeconds part of state to the new protocol instance for a
		// "gapless" handover
		retirementReport := RetirementReport{
			outcome.ValidAfterSeconds,
		}

		rwis = append(rwis, ocr3types.ReportWithInfo[commontypes.StreamsReportInfo]{
			Report: must(json.Marshal(retirementReport)),
			Info: commontypes.StreamsReportInfo{
				LifeCycleStage: outcome.LifeCycleStage,
				ReportFormat:   ReportFormatJSON,
			},
		})
	}

	for _, channelID := range outcome.ReportableChannels() {
		channelDefinition := outcome.ChannelDefinitions[channelID]
		values := []*big.Int{}
		for _, streamID := range channelDefinition.StreamIDs {
			values = append(values, outcome.StreamMedians[streamID])
		}

		report := Report{
			p.ConfigDigest,
			channelDefinition.ChainSelector,
			seqNr,
			channelID,
			outcome.ValidAfterSeconds[channelID],
			observationsTimestampSeconds,
			values,
			outcome.LifeCycleStage != LifeCycleStageProduction,
		}

		if encoded, err := p.encodeReport(report, channelDefinition.ReportFormat); err != nil {
			return nil, err
		} else {
			rwis = append(rwis, ocr3types.ReportWithInfo[commontypes.StreamsReportInfo]{
				Report: encoded,
				Info: commontypes.StreamsReportInfo{
					LifeCycleStage: outcome.LifeCycleStage,
					ReportFormat:   channelDefinition.ReportFormat,
				},
			})
		}
	}

	return rwis, nil
}

func (p *StreamsPlugin) ShouldAcceptAttestedReport(context.Context, uint64, ocr3types.ReportWithInfo[commontypes.StreamsReportInfo]) (bool, error) {
	// Transmit it all to the Mercury server
	return true, nil
}

func (p *StreamsPlugin) ShouldTransmitAcceptedReport(context.Context, uint64, ocr3types.ReportWithInfo[commontypes.StreamsReportInfo]) (bool, error) {
	// Transmit it all to the Mercury server
	return true, nil
}

// ObservationQuorum returns the minimum number of valid (according to
// ValidateObservation) observations needed to construct an outcome.
//
// This function should be pure. Don't do anything slow in here.
//
// This is an advanced feature. The "default" approach (what OCR1 & OCR2
// did) is to have an empty ValidateObservation function and return
// QuorumTwoFPlusOne from this function.
func (p *StreamsPlugin) ObservationQuorum(outctx ocr3types.OutcomeContext, query types.Query) (ocr3types.Quorum, error) {
	return ocr3types.QuorumTwoFPlusOne, nil
}

func (p *StreamsPlugin) Close() error {
	return nil
}

func subtractChannelDefinitions(minuend ChannelDefinitions, subtrahend ChannelDefinitions, limit int) ChannelDefinitions {
	differenceList := []ChannelDefinitionWithID{}
	for channelID, channelDefinition := range minuend {
		if _, ok := subtrahend[channelID]; !ok {
			differenceList = append(differenceList, ChannelDefinitionWithID{channelDefinition, channelID})
		}
	}

	// Sort so we return deterministic result
	sort.Slice(differenceList, func(i, j int) bool {
		return differenceList[i].ChannelID.Less(differenceList[j].ChannelID)
	})

	if len(differenceList) > limit {
		differenceList = differenceList[:limit]
	}

	difference := ChannelDefinitions{}
	for _, defWithID := range differenceList {
		difference[defWithID.ChannelID] = defWithID.ChannelDefinition
	}

	return difference
}
