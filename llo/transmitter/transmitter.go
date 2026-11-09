package transmitter

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	"golang.org/x/sync/errgroup"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	coretypes "github.com/smartcontractkit/chainlink-common/pkg/types/core"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/pluginconfig"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	mercurytransmitter "github.com/smartcontractkit/chainlink-data-streams/llo/transmitter/dataengine"
)

// LLO Transmitter implementation, based on
// core/services/relay/evm/mercury/transmitter.go
//
// If you need to "fan-out" transmits and send reports to a new destination,
// add a new subTransmitter

const (
	// Mercury server error codes
	DuplicateReport = 2
)

type TransmitNotifier interface {
	OnTransmit(listen func(digest types.ConfigDigest, seqNr uint64))
}

type Transmitter interface {
	llotypes.Transmitter
	services.Service
}

// batchTransmitter is a sub transmitter that can take the deliveries of one
// Merkle bundle at once, rather than one report per call.
type batchTransmitter interface {
	TransmitBatch(ctx context.Context, digest types.ConfigDigest, seqNr uint64, reports []ocr3types.ReportWithInfo[llotypes.ReportInfo], sigs []types.AttributedOnchainSignature) error
}

type TransmitterRetirementReportCacheWriter interface {
	StoreAttestedRetirementReport(ctx context.Context, cd ocrtypes.ConfigDigest, seqNr uint64, retirementReport []byte, sigs []types.AttributedOnchainSignature) error
}

type onTransmit struct {
	lggr      logger.Logger
	mu        sync.RWMutex
	listeners []func(digest types.ConfigDigest, seqNr uint64)
}

func (o *onTransmit) OnTransmit(listen func(digest types.ConfigDigest, seqNr uint64)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.listeners = append(o.listeners, listen)
}

func (o *onTransmit) notify(digest types.ConfigDigest, seqNr uint64) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for _, listener := range o.listeners {
		go o.notifyOne(listener, digest, seqNr)
	}
}

// notifyOne isolates a listener from the rest of the process: listeners run on
// their own goroutines, so an unrecovered panic in one would crash the plugin.
func (o *onTransmit) notifyOne(listener func(digest types.ConfigDigest, seqNr uint64), digest types.ConfigDigest, seqNr uint64) {
	defer func() {
		if r := recover(); r != nil {
			if o.lggr != nil {
				o.lggr.Errorw("Transmit listener panicked", "panic", r, "digest", digest, "seqNr", seqNr, "stacktrace", string(debug.Stack()))
			}
		}
	}()
	listener(digest, seqNr)
}

type transmitter struct {
	services.StateMachine
	lggr           logger.Logger
	verboseLogging bool
	fromAccount    string

	subTransmitters       []Transmitter
	retirementReportCache TransmitterRetirementReportCacheWriter
	*onTransmit
}

type TransmitterOpts struct {
	Lggr                   logger.Logger
	DonID                  uint32
	VerboseLogging         bool
	FromAccount            string
	MercuryTransmitterOpts *mercurytransmitter.Opts
	Subtransmitters        []pluginconfig.TransmitterConfig
	RetirementReportCache  TransmitterRetirementReportCacheWriter
	CapabilitiesRegistry   coretypes.CapabilitiesRegistry
}

// The transmitter will handle starting and stopping the subtransmitters
func NewTransmitter(opts TransmitterOpts) (Transmitter, error) {
	subTransmitters := []Transmitter{}

	if opts.MercuryTransmitterOpts != nil {
		subTransmitters = append(
			subTransmitters,
			mercurytransmitter.New(*opts.MercuryTransmitterOpts),
		)
	}
	return &transmitter{
		services.StateMachine{},
		opts.Lggr,
		opts.VerboseLogging,
		opts.FromAccount,
		subTransmitters,
		opts.RetirementReportCache,
		&onTransmit{lggr: opts.Lggr},
	}, nil
}

func (t *transmitter) Start(ctx context.Context) error {
	return t.StartOnce("llo.Transmitter", func() error {
		for _, st := range t.subTransmitters {
			if err := st.Start(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *transmitter) Close() error {
	return t.StopOnce("llo.Transmitter", func() error {
		for _, st := range t.subTransmitters {
			if err := st.Close(); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *transmitter) HealthReport() map[string]error {
	report := map[string]error{t.Name(): t.Healthy()}
	for _, st := range t.subTransmitters {
		services.CopyHealth(report, st.HealthReport())
	}
	return report
}

func (t *transmitter) Name() string { return t.lggr.Name() }

func (t *transmitter) Transmit(
	ctx context.Context,
	digest types.ConfigDigest,
	seqNr uint64,
	report ocr3types.ReportWithInfo[llotypes.ReportInfo],
	sigs []types.AttributedOnchainSignature,
) (err error) {
	if t.verboseLogging {
		t.lggr.Debugw("Transmit report", "digest", digest, "seqNr", seqNr, "report", report, "sigs", sigs)
	}

	if report.Info.ReportFormat == llotypes.ReportFormatRetirement {
		// Retirement reports don't get transmitted; rather, they are stored in
		// the RetirementReportCache
		t.lggr.Debugw("Storing retirement report", "digest", digest, "seqNr", seqNr)
		if err := t.retirementReportCache.StoreAttestedRetirementReport(ctx, digest, seqNr, report.Report, sigs); err != nil {
			return fmt.Errorf("failed to write retirement report to cache: %w", err)
		}
		return nil
	}
	t.notify(digest, seqNr)

	if protocol.IsStagingMarker(report.Info, report.Report) {
		// The staging marker is never transmitted. It exists to drive the
		// notification above, which flushes the telemetry buffered for seqNr.
		return nil
	}

	if protocol.IsMerkleFamily(report.Info.ReportFormat) {
		return t.transmitMerkleBundle(ctx, digest, seqNr, report, sigs)
	}

	g := new(errgroup.Group)
	for _, st := range t.subTransmitters {
		g.Go(func() error {
			return st.Transmit(ctx, digest, seqNr, report, sigs)
		})
	}
	return g.Wait()
}

// transmitMerkleBundle hands every delivery of an attested bundle to each sub
// transmitter. libocr calls Transmit once for the whole bundle, under one
// deadline, so a sub transmitter that takes a batch gets all of them in one
// call. One that does not gets them one at a time, and every delivery is
// attempted even when an earlier one fails.
func (t *transmitter) transmitMerkleBundle(ctx context.Context, digest types.ConfigDigest, seqNr uint64, report ocr3types.ReportWithInfo[llotypes.ReportInfo], sigs []types.AttributedOnchainSignature) error {
	reports, err := expandMerkleBundle(report)
	if err != nil {
		return err
	}

	g := new(errgroup.Group)
	for _, st := range t.subTransmitters {
		g.Go(func() error {
			if bt, ok := st.(batchTransmitter); ok {
				return bt.TransmitBatch(ctx, digest, seqNr, reports, sigs)
			}
			var errs error
			for _, r := range reports {
				errs = errors.Join(errs, st.Transmit(ctx, digest, seqNr, r, sigs))
			}
			return errs
		})
	}
	return g.Wait()
}

// expandMerkleBundle turns an attested bundle into one delivery per leaf. Each
// delivery carries the leaf and its proof to the root the signatures cover, so
// sub transmitters persist and send it like any other report, under the
// family's report format.
func expandMerkleBundle(report ocr3types.ReportWithInfo[llotypes.ReportInfo]) ([]ocr3types.ReportWithInfo[llotypes.ReportInfo], error) {
	bundle, err := protocol.LoadMerkleBundle(report.Report)
	if err != nil {
		return nil, fmt.Errorf("failed to expand %s bundle: %w", report.Info.ReportFormat, err)
	}
	if bundle.Family != report.Info.ReportFormat {
		return nil, fmt.Errorf("failed to expand %s bundle: bundle is of family %s", report.Info.ReportFormat, bundle.Family)
	}
	deliveries, err := bundle.Expand(report.Report)
	if err != nil {
		return nil, fmt.Errorf("failed to expand %s bundle: %w", report.Info.ReportFormat, err)
	}
	reports := make([]ocr3types.ReportWithInfo[llotypes.ReportInfo], len(deliveries))
	for i, delivery := range deliveries {
		encoded, err := protocol.EncodeMerkleDelivery(delivery)
		if err != nil {
			return nil, fmt.Errorf("failed to encode %s delivery: %w", report.Info.ReportFormat, err)
		}
		reports[i] = ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: encoded, Info: report.Info}
	}
	return reports, nil
}

// FromAccount returns the stringified (hex) CSA public key
func (t *transmitter) FromAccount(ctx context.Context) (ocr2types.Account, error) {
	return ocr2types.Account(t.fromAccount), nil
}
