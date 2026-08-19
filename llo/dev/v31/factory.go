package llo

import (
	"context"
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
	// BlobLifetimeRounds overrides DefaultBlobLifetimeRounds if non-zero. Must
	// be >1, since a snapshot gathered for one sequence number is consumed by a
	// later one.
	BlobLifetimeRounds uint64
	// MaxDurationBlobObservation overrides the pump's per-cycle observation
	// budget (default: DefaultBlobObservationDurationMultiplier *
	// cfg.MaxDurationObservation).
	MaxDurationBlobObservation time.Duration
	// MaxBlobSnapshotAge overrides the wall-clock age at which a parked snapshot
	// is discarded (default: DefaultBlobSnapshotAgeMultiplier *
	// cfg.MaxDurationObservation). A negative value disables the age check,
	// leaving blob expiry as the only staleness bound.
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
	l.Infow("llo/dev/v31.NewReportingPlugin", "onchainConfig", onchainConfig, "offchainConfig", offchainConfig)

	// Initialize the memory ballast
	protocol.InitMemoryBallast()

	blobLifetimeRounds := f.BlobLifetimeRounds
	if blobLifetimeRounds <= 1 {
		blobLifetimeRounds = DefaultBlobLifetimeRounds
	}
	if blobLifetimeRounds > MaxBlobLifetimeRounds {
		return nil, nil, fmt.Errorf("BlobLifetimeRounds (%d) exceeds MaxBlobLifetimeRounds (%d)", blobLifetimeRounds, MaxBlobLifetimeRounds)
	}
	blobObservationTimeout := f.MaxDurationBlobObservation
	if blobObservationTimeout <= 0 {
		blobObservationTimeout = DefaultBlobObservationDurationMultiplier * cfg.MaxDurationObservation
	}
	maxSnapshotAge := f.MaxBlobSnapshotAge
	switch {
	case maxSnapshotAge == 0:
		maxSnapshotAge = DefaultBlobSnapshotAgeMultiplier * cfg.MaxDurationObservation
	case maxSnapshotAge < 0:
		maxSnapshotAge = 0 // disabled; blob expiry still bounds staleness
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
	}

	// Definitions and the opts decoded from them are cached together, as one
	// immutable generation per c/seqnr, so a round can never mix the two.
	p.ChannelCache = protocol.NewChannelCache()

	// Setup the blobpump
	p.pump = newBlobPump(bbf, f.DataSource, l, cfg.ConfigDigest, f.Config.VerboseLogging, blobObservationTimeout, maxSnapshotAge, blobLifetimeRounds)
	p.pump.Start()

	unexpiredBlobCount := perOracleUnexpiredBlobCount(blobLifetimeRounds)
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

			MaxBlobPayloadBytes: ocr3_1types.MaxMaxBlobPayloadBytes,
			// Blobs live for blobLifetimeRounds sequence numbers and the pump
			// broadcasts about one per round, so both budgets are derived from
			// the configured lifetime plus a margin for asynchronous reaping
			// (see the libocr docs).
			MaxPerOracleUnexpiredBlobCount:                  unexpiredBlobCount,
			MaxPerOracleUnexpiredBlobCumulativePayloadBytes: unexpiredBlobCount * ocr3_1types.MaxMaxBlobPayloadBytes,
		},
	}
	if err := info.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid reporting plugin limits: %w", err)
	}

	return p, info, nil
}

// MaxMaxQueryBytesUnused documents that LLO uses an empty query.
const MaxMaxQueryBytesUnused = 0
