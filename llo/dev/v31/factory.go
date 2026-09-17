package llo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

var _ ocr3_1types.ReportingPluginFactory[llotypes.ReportInfo] = &PluginFactory{}

// PluginFactoryParams bundles the dependencies needed to construct the v31
// reporting plugin. It mirrors the v30 params, minus the outcome codec (state
// lives in the KeyValueState), and adds the blob pump knobs.
type PluginFactoryParams struct {
	Config
	protocol.PredecessorRetirementReportCache
	ShouldRetireCache
	protocol.RetirementReportCodec
	llotypes.ChannelDefinitionCache
	DataSource
	logger.Logger
	protocol.OnchainConfigCodec
	ReportCodecs map[llotypes.ReportFormat]protocol.ReportCodec
	// OutcomeTelemetryCh, if set, receives one telemetry struct per StateTransition.
	OutcomeTelemetryCh chan<- *protocol.LLOOutcomeTelemetry
	// ReportTelemetryCh, if set, receives one telemetry struct per emitted report.
	ReportTelemetryCh chan<- *protocol.LLOReportTelemetry
	// DonID is optional and used only for telemetry and logging.
	DonID uint32
	// MaxSnapshotRounds overrides DefaultMaxSnapshotRounds if non-zero. Bounds
	// how stale this node's own stream values may be when it references them,
	// and nothing else; it does not affect what peers can fetch. Must be >0,
	// since a snapshot gathered for one sequence number is consumed by a later
	// one, and must leave BlobFetchMarginRounds below BlobLifetimeRounds.
	MaxSnapshotRounds uint64
	// BlobLifetimeRounds overrides DefaultBlobLifetimeRounds if non-zero. Bounds
	// how long peers can still fetch a broadcast blob, and nothing else; it does
	// not bound staleness, MaxSnapshotRounds does.
	BlobLifetimeRounds uint64
	// MaxDurationBlobObservation overrides the pump's per-cycle observation
	// budget (default: DefaultBlobObservationDurationMultiplier *
	// cfg.MaxDurationObservation).
	MaxDurationBlobObservation time.Duration
	// MaxBlobSnapshotAge pins the wall-clock age at which a parked snapshot is
	// discarded. Left at zero the pump derives it from the round period it
	// measures, which is the only safe default: any bound derived from
	// MaxDurationObservation is unrelated to the round cadence and can reject
	// every snapshot. A negative value disables the check, leaving
	// MaxSnapshotRounds as the only staleness bound.
	MaxBlobSnapshotAge time.Duration
}

func NewPluginFactory(p PluginFactoryParams) *PluginFactory {
	return &PluginFactory{p}
}

type PluginFactory struct {
	PluginFactoryParams
}

func (f *PluginFactory) NewReportingPlugin(ctx context.Context, cfg ocr3types.ReportingPluginConfig, bbf ocr3_1types.BlobBroadcastFetcher) (ocr3_1types.ReportingPlugin[llotypes.ReportInfo], ocr3_1types.ReportingPluginInfo, error) {
	onchainConfig, err := f.OnchainConfigCodec.Decode(cfg.OnchainConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("NewReportingPlugin failed to decode onchain config; got: 0x%x (len: %d); %w", cfg.OnchainConfig, len(cfg.OnchainConfig), err)
	}
	offchainConfig, err := protocol.DecodeOffchainConfig(cfg.OffchainConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("NewReportingPlugin failed to decode offchain config; got: 0x%x (len: %d); %w", cfg.OffchainConfig, len(cfg.OffchainConfig), err)
	}

	l := logger.Sugared(f.Logger).With("lloProtocolVersion", offchainConfig.ProtocolVersion, "configDigest", cfg.ConfigDigest, "lloOCRVersion", "3.1")
	l.Infow("llo/dev/v31.NewReportingPlugin", "onchainConfig", onchainConfig, "offchainConfig", offchainConfig, "f", cfg.F, "n", cfg.N)

	// Initialize the memory ballast
	protocol.InitMemoryBallast()

	maxSnapshotRounds := f.MaxSnapshotRounds
	if maxSnapshotRounds == 0 {
		maxSnapshotRounds = DefaultMaxSnapshotRounds
	}
	blobLifetimeRounds := f.BlobLifetimeRounds
	if blobLifetimeRounds == 0 {
		blobLifetimeRounds = DefaultBlobLifetimeRounds
	}
	if blobLifetimeRounds > MaxBlobLifetimeRounds {
		return nil, nil, fmt.Errorf("BlobLifetimeRounds (%d) exceeds MaxBlobLifetimeRounds (%d)", blobLifetimeRounds, MaxBlobLifetimeRounds)
	}
	// A snapshot is last referenced at forSeqNr+maxSnapshotRounds-1 and its blob
	// expires at forSeqNr+blobLifetimeRounds, so this is the fetch margin.
	if blobLifetimeRounds+1 < maxSnapshotRounds+BlobFetchMarginRounds {
		return nil, nil, fmt.Errorf("BlobLifetimeRounds (%d) leaves less than %d rounds of fetch margin past MaxSnapshotRounds (%d)", blobLifetimeRounds, BlobFetchMarginRounds, maxSnapshotRounds)
	}
	// The contribution floor is a replicated state transition parameter, it
	// must come from the offchainConfig and set explicitly.
	if offchainConfig.AggregationFaultTolerance == nil {
		return nil, nil, errors.New("NewReportingPlugin: offchain config must set aggregationFaultTolerance explicitly")
	}
	aggregationFaultTolerance := int(*offchainConfig.AggregationFaultTolerance)
	if aggregationFaultTolerance > cfg.F {
		return nil, nil, fmt.Errorf("aggregationFaultTolerance (%d) must not exceed consensus F (%d): a floor of %d contributions can never be met from %d observations",
			aggregationFaultTolerance, cfg.F, 2*aggregationFaultTolerance+1, 2*cfg.F+1)
	}

	blobObservationTimeout := f.MaxDurationBlobObservation
	if blobObservationTimeout <= 0 {
		blobObservationTimeout = DefaultBlobObservationDurationMultiplier * cfg.MaxDurationObservation
	}

	p := &Plugin{
		Config:                              f.Config,
		PredecessorConfigDigest:             onchainConfig.PredecessorConfigDigest,
		ConfigDigest:                        cfg.ConfigDigest,
		PredecessorRetirementReportCache:    f.PredecessorRetirementReportCache,
		ShouldRetireCache:                   f.ShouldRetireCache,
		ChannelDefinitionCache:              f.ChannelDefinitionCache,
		DataSource:                          f.DataSource,
		Logger:                              l,
		N:                                   cfg.N,
		F:                                   cfg.F,
		RetirementReportCodec:               f.RetirementReportCodec,
		ReportCodecs:                        f.ReportCodecs,
		DonID:                               f.DonID,
		OutcomeTelemetryCh:                  f.OutcomeTelemetryCh,
		ReportTelemetryCh:                   f.ReportTelemetryCh,
		ProtocolVersion:                     offchainConfig.ProtocolVersion,
		DefaultMinReportIntervalNanoseconds: offchainConfig.DefaultMinReportIntervalNanoseconds,
		AggregationFaultTolerance:           aggregationFaultTolerance,
	}

	// Definitions and the opts decoded from them are cached together, as one
	// immutable generation per c/seqnr, so a round can never mix the two.
	p.ChannelCache = protocol.NewChannelCache()

	// Setup the blobpump
	p.pump = newBlobPump(l, blobPumpParams{
		bbf:                bbf,
		ds:                 f.DataSource,
		configDigest:       cfg.ConfigDigest,
		verboseLogging:     f.Config.VerboseLogging,
		observationTimeout: blobObservationTimeout,
		maxSnapshotAge:     f.MaxBlobSnapshotAge,
		maxSnapshotRounds:  maxSnapshotRounds,
		blobLifetimeRounds: blobLifetimeRounds,
	})
	p.pump.Start()

	unexpiredBlobCount := perOracleUnexpiredBlobCount(blobLifetimeRounds)
	// Declared limits. Each is the libocr maximum, which is only honest if the
	// plugin's own admission rules keep what it produces underneath it: libocr
	// rejects an oversized message or write set, which fails the round for every
	// oracle. The derivations, and which of them are currently enforced, are:
	//
	//	MaxObservationBytes           bounded post-decompression by
	//	                              protocol.MaxDecompressedObservationLength,
	//	                              itself derived from
	//	                              MaxObservationStreamValuesLength stream
	//	                              values plus
	//	                              MaxObservationUpdateChannelDefinitionsLength
	//	                              definitions.
	//	MaxReportsPlusPrecursorBytes  the precursor embeds every definition plus
	//	                              every stream aggregate. The aggregate count
	//	                              is bounded by protocol.MaxPersistedAggregates,
	//	                              and the definition set by channel count
	//	                              (MaxOutcomeChannelDefinitionsLength), total
	//	                              stream entries (MaxTotalStreamEntries) and
	//	                              opts bytes (MaxTotalOptsBytes) at admission.
	//                                A stream value's decimal coefficient is bounded
	//	                              on observation decode.
	//	MaxKeyValueModifiedKeys*      the per-round write set is c/defs plus r/agg
	//	                              plus the history windows, and history is
	//	                              held to protocol.MaxHistoryTotalBytes so it
	//	                              cannot consume the whole budget on its own.
	//	MaxBlobPayloadBytes           enforced on the write side by
	//	                              encodeBlobPayload.
	info := ocr3_1types.ReportingPluginInfo1{
		Name: "LLO-3.1",
		Limits: ocr3_1types.ReportingPluginLimits{
			MaxQueryBytes:                MaxMaxQueryBytesUnused,
			MaxObservationBytes:          ocr3_1types.MaxMaxObservationBytes,
			MaxReportsPlusPrecursorBytes: ocr3_1types.MaxMaxReportsPlusPrecursorBytes,
			MaxReportBytes:               ocr3_1types.MaxMaxReportBytes,
			MaxReportCount:               protocol.MaxReportCount,

			MaxKeyValueModifiedKeys:                ocr3_1types.MaxMaxKeyValueModifiedKeys,
			MaxKeyValueModifiedKeysPlusValuesBytes: ocr3_1types.MaxMaxKeyValueModifiedKeysPlusValuesBytes,

			MaxBlobPayloadBytes: MaxBlobPayloadBytes,
			// Blobs live for blobLifetimeRounds sequence numbers and the pump
			// broadcasts about one per round, so both budgets are derived from
			// the configured lifetime plus a margin for asynchronous reaping
			// (see the libocr docs).
			MaxPerOracleUnexpiredBlobCount:                  unexpiredBlobCount,
			MaxPerOracleUnexpiredBlobCumulativePayloadBytes: unexpiredBlobCount * MaxBlobPayloadBytes,
		},
	}
	if err := info.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid reporting plugin limits: %w", err)
	}

	return p, info, nil
}

// MaxMaxQueryBytesUnused documents that LLO uses an empty query.
const MaxMaxQueryBytesUnused = 0
