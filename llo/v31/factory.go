package llo

import (
	"context"
	"fmt"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// DefaultBlobThreshold is the default serialized stream-value payload size in
// bytes above which an observation offloads its stream values to a blob.
const DefaultBlobThreshold = 128 * 1024

var _ ocr3_1types.ReportingPluginFactory[llotypes.ReportInfo] = &PluginFactory{}

// PluginFactoryParams bundles the dependencies needed to construct the v31
// reporting plugin. It mirrors the v30 params, minus the outcome codec (state
// lives in the KeyValueState), and adds BlobThreshold.
type PluginFactoryParams struct {
	Config
	llocommon.PredecessorRetirementReportCache
	ShouldRetireCache
	llocommon.RetirementReportCodec
	llotypes.ChannelDefinitionCache
	DataSource
	logger.Logger
	llocommon.OnchainConfigCodec
	ReportCodecs map[llotypes.ReportFormat]llocommon.ReportCodec
	// DonID is optional and used only for telemetry and logging.
	DonID uint32
	// BlobThreshold overrides DefaultBlobThreshold if non-zero. A negative value
	// disables blob offloading.
	BlobThreshold int
}

func NewPluginFactory(p PluginFactoryParams) *PluginFactory {
	return &PluginFactory{p}
}

type PluginFactory struct {
	PluginFactoryParams
}

func (f *PluginFactory) NewReportingPlugin(ctx context.Context, cfg ocr3types.ReportingPluginConfig, _ ocr3_1types.BlobBroadcastFetcher) (ocr3_1types.ReportingPlugin[llotypes.ReportInfo], ocr3_1types.ReportingPluginInfo, error) {
	onchainConfig, err := f.OnchainConfigCodec.Decode(cfg.OnchainConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("NewReportingPlugin failed to decode onchain config; got: 0x%x (len: %d); %w", cfg.OnchainConfig, len(cfg.OnchainConfig), err)
	}
	offchainConfig, err := llocommon.DecodeOffchainConfig(cfg.OffchainConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("NewReportingPlugin failed to decode offchain config; got: 0x%x (len: %d); %w", cfg.OffchainConfig, len(cfg.OffchainConfig), err)
	}

	l := logger.Sugared(f.Logger).With("lloProtocolVersion", offchainConfig.ProtocolVersion, "configDigest", cfg.ConfigDigest, "lloOCRVersion", "3.1")
	l.Infow("llo/v31.NewReportingPlugin", "onchainConfig", onchainConfig, "offchainConfig", offchainConfig)

	blobThreshold := f.BlobThreshold
	switch {
	case blobThreshold == 0:
		blobThreshold = DefaultBlobThreshold
	case blobThreshold < 0:
		blobThreshold = 0 // disabled
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
		OptsCache:                           llocommon.NewOptsCache(),
		MaxDurationObservation:              cfg.MaxDurationObservation,
		ProtocolVersion:                     offchainConfig.ProtocolVersion,
		DefaultMinReportIntervalNanoseconds: offchainConfig.DefaultMinReportIntervalNanoseconds,
		BlobThreshold:                       blobThreshold,
	}

	info := ocr3_1types.ReportingPluginInfo1{
		Name: "LLO-3.1",
		Limits: ocr3_1types.ReportingPluginLimits{
			MaxQueryBytes:                MaxMaxQueryBytesUnused,
			MaxObservationBytes:          ocr3_1types.MaxMaxObservationBytes,
			MaxReportsPlusPrecursorBytes: ocr3_1types.MaxMaxReportsPlusPrecursorBytes,
			MaxReportBytes:               ocr3_1types.MaxMaxReportBytes,
			MaxReportCount:               llocommon.MaxReportCount,

			MaxKeyValueModifiedKeys:                ocr3_1types.MaxMaxKeyValueModifiedKeys,
			MaxKeyValueModifiedKeysPlusValuesBytes: ocr3_1types.MaxMaxKeyValueModifiedKeysPlusValuesBytes,

			MaxBlobPayloadBytes: ocr3_1types.MaxMaxBlobPayloadBytes,
			// Blobs expire after a couple of sequence numbers; set loosely to
			// account for asynchronous reaping (see the libocr docs).
			MaxPerOracleUnexpiredBlobCount:                  32,
			MaxPerOracleUnexpiredBlobCumulativePayloadBytes: 32 * ocr3_1types.MaxMaxBlobPayloadBytes,
		},
	}
	if err := info.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid reporting plugin limits: %w", err)
	}

	return p, info, nil
}

// MaxMaxQueryBytesUnused documents that LLO uses an empty query.
const MaxMaxQueryBytesUnused = 0
