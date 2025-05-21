package nats

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type TransmitterOpts struct {
	Logger       logger.Logger
	FromAccount  string
	DonID        uint32
	ServerURLs   []string
	ClientSigner crypto.Signer
	ServerPubKey ed25519.PublicKey
}

type transmitter struct {
	services.StateMachine
	services.Service

	lggr        logger.Logger
	fromAccount string
	donID       uint32
	serverURL   []string
	client      Client

	// Reusable hash instance with mutex for thread safety
	hashPool sync.Pool
}

func NewTransmitter(opts TransmitterOpts) (llotypes.Transmitter, error) {
	t := &transmitter{
		lggr:        opts.Logger,
		fromAccount: opts.FromAccount,
		donID:       opts.DonID,
		serverURL:   opts.ServerURLs,
	}

	// Initialize the hash pool
	t.hashPool.New = func() interface{} {
		return xxhash.New()
	}

	// Create NATS client
	clientOpts := ClientOpts{
		Logger:       opts.Logger,
		ClientSigner: opts.ClientSigner,
		ServerPubKey: opts.ServerPubKey,
		ServerURLs:   opts.ServerURLs,
	}
	client, err := NewClient(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create NATS client: %w", err)
	}
	t.client = client

	// Initialize service with subservices
	svc, _ := services.Config{
		Name:  "NATSTransmitter",
		Start: func(ctx context.Context) error { return nil },
		Close: func() error { return nil },
		NewSubServices: func(lggr logger.Logger) []services.Service {
			return []services.Service{client}
		},
	}.NewServiceEngine(opts.Logger)
	t.Service = svc

	return t, nil
}

func (t *transmitter) Transmit(
	ctx context.Context,
	digest ocr2types.ConfigDigest,
	seqNr uint64,
	report ocr3types.ReportWithInfo[llotypes.ReportInfo],
	sigs []ocr2types.AttributedOnchainSignature,
) error {
	if !t.IfStarted(func() {}) {
		return fmt.Errorf("transmitter is not started")
	}

	// Get a hash instance from the pool
	h := t.hashPool.Get().(*xxhash.Digest)
	defer t.hashPool.Put(h)

	// Reset the hash instance before use
	h.Reset()
	h.Write(report.Report)
	dedupeKey := hex.EncodeToString(h.Sum(nil))

	err := t.client.Transmit(ctx, report.Report, dedupeKey, t.donID)
	if err != nil {
		t.lggr.Errorw("Failed to transmit report",
			"error", err,
			"digest", digest,
			"seqNr", seqNr,
			"reportFormat", report.Info.ReportFormat,
		)
		return err
	}

	t.lggr.Debugw("Successfully transmitted report",
		"digest", digest,
		"seqNr", seqNr,
		"reportFormat", report.Info.ReportFormat,
	)
	return nil
}

func (t *transmitter) FromAccount(ctx context.Context) (ocr2types.Account, error) {
	return ocr2types.Account(t.fromAccount), nil
}

func (t *transmitter) Ready() error {
	return t.Healthy()
}

func (t *transmitter) HealthReport() map[string]error {
	report := map[string]error{t.Name(): t.Healthy()}
	services.CopyHealth(report, t.client.HealthReport())
	return report
}

func (t *transmitter) Name() string {
	return t.lggr.Name()
}
