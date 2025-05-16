package llo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/smartcontractkit/libocr/quorumhelper"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// Additional limits so we can more effectively bound the size of observations
// NOTE: These are hardcoded because these exact values are relied upon as a
// property of coming to consensus, it's too dangerous to make these
// configurable on a per-node basis. It may be possible to add them to the
// OffchainConfig if they need to be changed dynamically and in a
// backwards-compatible way.
const (
	// OCR protocol limits
	// NOTE: CAREFUL! If we ever accidentally exceed these e.g.
	// through too many channels/streams, the protocol will halt.
	//
	// TODO: How many channels/streams can we support given these constraints?
	// https://smartcontract-it.atlassian.net/browse/MERC-6468
	MaxReportCount       = ocr3types.MaxMaxReportCount
	MaxObservationLength = ocr3types.MaxMaxObservationLength
	MaxOutcomeLength     = ocr3types.MaxMaxOutcomeLength
	MaxReportLength      = ocr3types.MaxMaxReportLength

	// LLO-specific limits
	//
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

type DSOpts interface {
	VerboseLogging() bool
	SeqNr() uint64
	OutCtx() ocr3types.OutcomeContext
	ConfigDigest() ocr2types.ConfigDigest
	ObservationTimestamp() time.Time
	OutcomeCodec() OutcomeCodec
}

type dsOpts struct {
	verboseLogging               bool
	outCtx                       ocr3types.OutcomeContext
	configDigest                 ocr2types.ConfigDigest
	outcomeCodec                 OutcomeCodec
	observationTimestamp         time.Time
	protocolVersion              uint32
	minReportIntervalNanoseconds uint64
}

func (o *dsOpts) VerboseLogging() bool {
	return o.verboseLogging
}

func (o *dsOpts) SeqNr() uint64 {
	return o.outCtx.SeqNr
}

func (o *dsOpts) OutCtx() ocr3types.OutcomeContext {
	return o.outCtx
}

func (o *dsOpts) ConfigDigest() ocr2types.ConfigDigest {
	return o.configDigest
}

func (o *dsOpts) ObservationTimestamp() time.Time {
	return o.observationTimestamp
}

func (o *dsOpts) OutcomeCodec() OutcomeCodec {
	return o.outcomeCodec
}

func (o *dsOpts) ProtocolVersion() uint32 {
	return o.protocolVersion
}

func (o *dsOpts) MinReportIntervalNanoseconds() uint64 {
	return o.minReportIntervalNanoseconds
}

type DataSource interface {
	// For each known streamID, Observe should set the observed value in the
	// passed streamValues.
	// If an observation fails, or the stream is unknown, no value should be
	// set.
	Observe(ctx context.Context, streamValues StreamValues, opts DSOpts) error
}

// Protocol instances start in either the staging or production stage. They
// may later be retired and "hand over" their work to another protocol instance
// that will move from the staging to the production stage.
const (
	LifeCycleStageStaging    llotypes.LifeCycleStage = "staging"
	LifeCycleStageProduction llotypes.LifeCycleStage = "production"
	LifeCycleStageRetired    llotypes.LifeCycleStage = "retired"
)

type RetirementReport struct {
	// Retirement reports are not guaranteed to be compatible across different
	// protocol versions
	ProtocolVersion uint32
	// Carries validity time stamps between protocol instances to ensure there
	// are no gaps
	ValidAfterNanoseconds map[llotypes.ChannelID]uint64
}

type ShouldRetireCache interface { // reads asynchronously from onchain ConfigurationStore
	// Should the protocol instance retire according to the configuration
	// contract?
	// See: https://github.com/smartcontractkit/mercury-v1-sketch/blob/main/onchain/src/ConfigurationStore.sol#L18
	ShouldRetire(digest ocr2types.ConfigDigest) (bool, error)
}

// The predecessor protocol instance stores its attested retirement report in
// this cache (locally, offchain), so it can be fetched by the successor
// protocol instance.
//
// PredecessorRetirementReportCache is populated by the old protocol instance
// writing to it and the new protocol instance reading from it.
//
// The sketch envisions it being implemented as a single object that is shared
// between different protocol instances.
type PredecessorRetirementReportCache interface {
	// AttestedRetirementReport returns the attested retirement report for the
	// given config digest from the local cache.
	//
	// This should return nil and not error in the case of a missing attested
	// retirement report.
	AttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest) ([]byte, error)
	// CheckAttestedRetirementReport verifies that an attested retirement
	// report, which may have come from another node, is valid (signed) with
	// signers corresponding to the given config digest
	CheckAttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest, attestedRetirementReport []byte) (RetirementReport, error)
}

type ChannelDefinitionCache interface {
	Definitions() llotypes.ChannelDefinitions
}

// A ReportingPlugin allows plugging custom logic into the OCR3 protocol. The OCR
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
// A ReportingPlugin instance will only ever serve a single protocol instance.
var _ ocr3types.ReportingPluginFactory[llotypes.ReportInfo] = &PluginFactory{}

type PluginFactoryParams struct {
	Config
	PredecessorRetirementReportCache
	ShouldRetireCache
	RetirementReportCodec
	ChannelDefinitionCache
	DataSource
	logger.Logger
	OnchainConfigCodec
	ReportCodecs map[llotypes.ReportFormat]ReportCodec
	// LLOOutcomeTelemetryCh if set will be used to send one telemetry struct per
	// round in the Outcome stage
	OutcomeTelemetryCh chan<- *LLOOutcomeTelemetry
	// ReportTelemetryCh if set will be used to send one telemetry struct per
	// transmissible report in the Report stage
	ReportTelemetryCh chan<- *LLOReportTelemetry
	// DonID is optional and used only for telemetry and logging
	DonID uint32
}

func NewPluginFactory(p PluginFactoryParams) *PluginFactory {
	return &PluginFactory{p}
}

type Config struct {
	// Enables additional logging that might be expensive, e.g. logging entire
	// channel definitions on every round or other very large structs
	VerboseLogging bool
}

type PluginFactory struct {
	PluginFactoryParams
}

func (f *PluginFactory) NewReportingPlugin(ctx context.Context, cfg ocr3types.ReportingPluginConfig) (ocr3types.ReportingPlugin[llotypes.ReportInfo], ocr3types.ReportingPluginInfo, error) {
	onchainConfig, err := f.OnchainConfigCodec.Decode(cfg.OnchainConfig)
	if err != nil {
		return nil, ocr3types.ReportingPluginInfo{}, fmt.Errorf("NewReportingPlugin failed to decode onchain config; got: 0x%x (len: %d); %w", cfg.OnchainConfig, len(cfg.OnchainConfig), err)
	}
	offchainConfig, err := DecodeOffchainConfig(cfg.OffchainConfig)
	if err != nil {
		return nil, ocr3types.ReportingPluginInfo{}, fmt.Errorf("NewReportingPlugin failed to decode offchain config; got: 0x%x (len: %d); %w", cfg.OffchainConfig, len(cfg.OffchainConfig), err)
	}

	l := logger.Sugared(f.Logger).With("lloProtocolVersion", offchainConfig.ProtocolVersion, "configDigest", cfg.ConfigDigest)

	l.Infow("llo.NewReportingPlugin", "onchainConfig", onchainConfig, "offchainConfig", offchainConfig)

	return &Plugin{
			f.Config,
			onchainConfig.PredecessorConfigDigest,
			cfg.ConfigDigest,
			f.PredecessorRetirementReportCache,
			f.ShouldRetireCache,
			f.ChannelDefinitionCache,
			f.DataSource,
			l,
			cfg.N,
			cfg.F,
			protoObservationCodec{},
			offchainConfig.GetOutcomeCodec(),
			f.RetirementReportCodec,
			f.ReportCodecs,
			f.OutcomeTelemetryCh,
			f.ReportTelemetryCh,
			f.DonID,
			cfg.MaxDurationObservation,
			offchainConfig.ProtocolVersion,
			offchainConfig.DefaultMinReportIntervalNanoseconds,
		}, ocr3types.ReportingPluginInfo{
			Name: "LLO",
			Limits: ocr3types.ReportingPluginLimits{
				MaxQueryLength:       0,
				MaxObservationLength: MaxObservationLength,
				MaxOutcomeLength:     MaxOutcomeLength,
				MaxReportLength:      MaxReportLength,
				MaxReportCount:       MaxReportCount,
			},
		}, nil
}

var _ ocr3types.ReportingPlugin[llotypes.ReportInfo] = &Plugin{}

type Plugin struct {
	Config                           Config
	PredecessorConfigDigest          *types.ConfigDigest
	ConfigDigest                     types.ConfigDigest
	PredecessorRetirementReportCache PredecessorRetirementReportCache
	ShouldRetireCache                ShouldRetireCache
	ChannelDefinitionCache           ChannelDefinitionCache
	DataSource                       DataSource
	Logger                           logger.Logger
	N                                int
	F                                int
	ObservationCodec                 ObservationCodec
	OutcomeCodec                     OutcomeCodec
	RetirementReportCodec            RetirementReportCodec
	ReportCodecs                     map[llotypes.ReportFormat]ReportCodec
	OutcomeTelemetryCh               chan<- *LLOOutcomeTelemetry
	ReportTelemetryCh                chan<- *LLOReportTelemetry
	DonID                            uint32

	// From ReportingPluginConfig
	MaxDurationObservation time.Duration

	// From offchain config
	ProtocolVersion                     uint32
	DefaultMinReportIntervalNanoseconds uint64
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
func (p *Plugin) Query(ctx context.Context, outctx ocr3types.OutcomeContext) (types.Query, error) {
	return nil, nil
}

// Observation gets an observation from the underlying data source. Returns
// a value or an error.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
//
// Should return a serialized Observation struct.
func (p *Plugin) Observation(ctx context.Context, outctx ocr3types.OutcomeContext, query types.Query) (types.Observation, error) {
	return p.observation(ctx, outctx, query)
}

// Should return an error if an observation isn't well-formed.
// Non-well-formed  observations will be discarded by the protocol. This is
// called for each observation, don't do anything slow in here.
//
// You may assume that the outctx.SeqNr is increasing monotonically (though
// *not* strictly) across the lifetime of a protocol instance and that
// outctx.previousOutcome contains the consensus outcome with sequence
// number (outctx.SeqNr-1).
func (p *Plugin) ValidateObservation(ctx context.Context, outctx ocr3types.OutcomeContext, query types.Query, ao types.AttributedObservation) error {
	if outctx.SeqNr < 1 {
		return fmt.Errorf("Invalid SeqNr: %d", outctx.SeqNr)
	} else if outctx.SeqNr == 1 {
		if len(ao.Observation) != 0 {
			return fmt.Errorf("Expected empty observation for first round, got: 0x%x", ao.Observation)
		}
	}

	observation, err := p.ObservationCodec.Decode(ao.Observation)
	if err != nil {
		return fmt.Errorf("Observation decode error (got: 0x%x): %w", ao.Observation, err)
	}

	if p.PredecessorConfigDigest == nil && len(observation.AttestedPredecessorRetirement) != 0 {
		return errors.New("AttestedPredecessorRetirement is not empty even though this instance has no predecessor")
	}

	if len(observation.UpdateChannelDefinitions) > MaxObservationUpdateChannelDefinitionsLength {
		return fmt.Errorf("UpdateChannelDefinitions is too long: %v vs %v", len(observation.UpdateChannelDefinitions), MaxObservationUpdateChannelDefinitionsLength)
	}

	if len(observation.RemoveChannelIDs) > MaxObservationRemoveChannelIDsLength {
		return fmt.Errorf("RemoveChannelIDs is too long: %v vs %v", len(observation.RemoveChannelIDs), MaxObservationRemoveChannelIDsLength)
	}

	if err := VerifyChannelDefinitions(p.ReportCodecs, observation.UpdateChannelDefinitions); err != nil {
		return fmt.Errorf("UpdateChannelDefinitions is invalid: %w", err)
	}

	if len(observation.StreamValues) > MaxObservationStreamValuesLength {
		return fmt.Errorf("StreamValues is too long: %v vs %v", len(observation.StreamValues), MaxObservationStreamValuesLength)
	}

	for _, streamValue := range observation.StreamValues {
		switch v := streamValue.(type) {
		case *TimestampedStreamValue:
			switch v.StreamValue.Type() {
			case LLOStreamValue_Decimal:
			default:
				return fmt.Errorf("nested stream value on TimestampedStreamValue must be a Decimal, got: %v", v.StreamValue.Type())
			}
			// TODO: verify that TimestampedStreamValues are not ridiculously far into the future?
		default:
			// Can add additional type-specific validation here, if needed
		}
	}

	return nil
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
//
// libocr guarantees that this will always be called with at least 2f+1
// AttributedObservations
func (p *Plugin) Outcome(ctx context.Context, outctx ocr3types.OutcomeContext, query types.Query, aos []types.AttributedObservation) (ocr3types.Outcome, error) {
	return p.outcome(outctx, query, aos)
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
func (p *Plugin) Reports(ctx context.Context, seqNr uint64, rawOutcome ocr3types.Outcome) ([]ocr3types.ReportPlus[llotypes.ReportInfo], error) {
	return p.reports(ctx, seqNr, rawOutcome)
}

func (p *Plugin) ShouldAcceptAttestedReport(context.Context, uint64, ocr3types.ReportWithInfo[llotypes.ReportInfo]) (bool, error) {
	// Transmit it all to the Mercury server
	return true, nil
}

func (p *Plugin) ShouldTransmitAcceptedReport(context.Context, uint64, ocr3types.ReportWithInfo[llotypes.ReportInfo]) (bool, error) {
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
func (p *Plugin) ObservationQuorum(ctx context.Context, outctx ocr3types.OutcomeContext, query types.Query, aos []types.AttributedObservation) (bool, error) {
	return quorumhelper.ObservationCountReachesObservationQuorum(quorumhelper.QuorumTwoFPlusOne, p.N, p.F, aos), nil
}

func (p *Plugin) Close() error {
	return nil
}
