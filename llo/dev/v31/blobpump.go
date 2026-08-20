package llo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/datasource"
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	"google.golang.org/protobuf/proto"
)

// Defaults for the blob pump. See PluginFactoryParams for the overrides.
const (
	// DefaultBlobLifetimeRounds is how many sequence numbers past the one a
	// pump cycle started for a broadcast blob is hinted to live. It must exceed
	// 1: a snapshot gathered for seqNr N is normally consumed at N+1, and the
	// blob is fetched by the other oracles during that later round.
	DefaultBlobLifetimeRounds = 3
	// DefaultBlobObservationDurationMultiplier scales MaxDurationObservation
	// into the pump's per-cycle budget. The pump runs off the OCR critical
	// path, so it can afford to wait longer than a synchronous observation.
	DefaultBlobObservationDurationMultiplier = 2
	// DefaultBlobSnapshotAgeMultiplier scales MaxDurationObservation into the
	// wall-clock age at which a parked snapshot is discarded. Generous on
	// purpose: ordinary jitter should reuse the previous snapshot rather than
	// discard it, since blob expiry already bounds staleness in rounds.
	DefaultBlobSnapshotAgeMultiplier = 5
	// MaxBlobLifetimeRounds bounds BlobLifetimeRounds. The pump broadcasts
	// roughly one blob per round, so the unexpired-blob budget declared to
	// libocr grows with the lifetime; this keeps that budget sane.
	MaxBlobLifetimeRounds = 64
	// BlobReapingMarginRounds is added to blobLifetimeRounds when deriving the
	// per-oracle unexpired-blob budget, covering blobs that are expired but not
	// yet reaped (reaping is asynchronous, on the order of tens of seconds).
	BlobReapingMarginRounds = 16
	// MinPerOracleUnexpiredBlobCount is the floor for the derived budget.
	MinPerOracleUnexpiredBlobCount = 32
)

// perOracleUnexpiredBlobCount derives the per-oracle unexpired-blob budget from
// the configured blob lifetime: one blob per round, plus a reaping margin.
func perOracleUnexpiredBlobCount(blobLifetimeRounds uint64) int {
	n := int(blobLifetimeRounds) + BlobReapingMarginRounds
	return max(n, MinPerOracleUnexpiredBlobCount)
}

// pumpInput is the round context the pump needs, published by Observation.
type pumpInput struct {
	streams        []llotypes.StreamID
	seqNr          uint64
	lifeCycleStage llotypes.LifeCycleStage
}

// blobSnapshot is one completed pump cycle: stream values already serialized,
// broadcast as a blob, and reduced to the marshaled handle that goes on the
// wire.
type blobSnapshot struct {
	handleBytes []byte
	observedAt  time.Time
	// forSeqNr is the sequence number known when the cycle started.
	forSeqNr uint64
	// expiresAt is the blob expiration hint; the snapshot must not be used at
	// or beyond this sequence number.
	expiresAt uint64
	// streamCount is the number of streams the cycle observed (for logging).
	streamCount int
}

// blobPump gathers stream observations off the OCR critical path and broadcasts
// them as blobs, parking the resulting handle for Observation to pick up.
//
// Cadence is consumption-driven: a cycle is kicked whenever Observation takes
// (or discards) a snapshot, so the pump rate tracks the round rate without
// needing to know deltaRound, and no blob is broadcast that no round asked for.
// Cycles are serial, so there is never more than one DataSource.Observe in
// flight. There is deliberately no idle watchdog: a snapshot's usability is
// bounded by sequence number, so refreshing while no rounds are running would
// produce snapshots that are already too old to use.
type blobPump struct {
	bbf                ocr3_1types.BlobBroadcastFetcher
	ds                 DataSource
	lggr               logger.Logger
	configDigest       ocrtypes.ConfigDigest
	verboseLogging     bool
	observationTimeout time.Duration
	maxSnapshotAge     time.Duration
	blobLifetimeRounds uint64

	trigger chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	inFlight atomic.Bool
	misses   atomic.Uint64
	cycles   atomic.Uint64

	mu    sync.Mutex
	input pumpInput
	ready *blobSnapshot
}

func newBlobPump(
	bbf ocr3_1types.BlobBroadcastFetcher,
	ds DataSource,
	lggr logger.Logger,
	configDigest ocrtypes.ConfigDigest,
	verboseLogging bool,
	observationTimeout time.Duration,
	maxSnapshotAge time.Duration,
	blobLifetimeRounds uint64,
) *blobPump {
	ctx, cancel := context.WithCancel(context.Background())
	return &blobPump{
		bbf:                bbf,
		ds:                 ds,
		lggr:               logger.Sugared(lggr).Named("BlobPump"),
		configDigest:       configDigest,
		verboseLogging:     verboseLogging,
		observationTimeout: observationTimeout,
		maxSnapshotAge:     maxSnapshotAge,
		blobLifetimeRounds: blobLifetimeRounds,
		trigger:            make(chan struct{}, 1),
		ctx:                ctx,
		cancel:             cancel,
	}
}

// enabled reports whether the pump can actually produce snapshots. Both
// dependencies come from the host: a nil BlobBroadcastFetcher (as passed by
// harnesses that run the plugin without blob transport) or a nil DataSource
// leaves the pump inert rather than panicking in the loop goroutine.
func (p *blobPump) enabled() bool { return p.bbf != nil && p.ds != nil }

// Start launches the pump loop. Safe to call once. A pump with no broadcaster or
// no data source starts no goroutine.
func (p *blobPump) Start() {
	if !p.enabled() {
		p.lggr.Warnw("Blob pump disabled; observations will carry no stream values", "hasBroadcaster", p.bbf != nil, "hasDataSource", p.ds != nil)
		return
	}
	p.wg.Add(1)
	go p.run()
}

// Close stops the pump and waits for any in-flight cycle to unwind.
func (p *blobPump) Close() {
	p.cancel()
	p.wg.Wait()
}

// SetInput publishes the round context for subsequent cycles. Cheap; called
// from Observation before Take.
func (p *blobPump) SetInput(in pumpInput) {
	p.mu.Lock()
	p.input = in
	p.mu.Unlock()
}

// Take returns the parked snapshot if it is still usable at seqNr, and always
// kicks the next cycle: kicking on a discard as well as on a hit is what stops
// a single unusable snapshot from stalling the pump forever. The second return
// value is the reason a snapshot was not returned, for logging.
func (p *blobPump) Take(seqNr uint64) (*blobSnapshot, string) {
	if !p.enabled() {
		p.misses.Add(1)
		return nil, "blob pump disabled"
	}

	p.mu.Lock()
	snap := p.ready
	p.ready = nil
	p.mu.Unlock()

	defer p.kick()

	switch {
	case snap == nil:
		p.misses.Add(1)
		if p.inFlight.Load() {
			return nil, "cycle in flight"
		}
		return nil, "no snapshot parked"
	case seqNr >= snap.expiresAt:
		p.misses.Add(1)
		return nil, fmt.Sprintf("blob expired (forSeqNr=%d expiresAt=%d)", snap.forSeqNr, snap.expiresAt)
	case p.maxSnapshotAge > 0 && time.Since(snap.observedAt) > p.maxSnapshotAge:
		p.misses.Add(1)
		return nil, fmt.Sprintf("snapshot too old (age=%s max=%s)", time.Since(snap.observedAt), p.maxSnapshotAge)
	default:
		return snap, ""
	}
}

// Misses reports how many rounds found no usable snapshot.
func (p *blobPump) Misses() uint64 { return p.misses.Load() }

// Cycles reports how many snapshots the pump has successfully parked.
func (p *blobPump) Cycles() uint64 { return p.cycles.Load() }

func (p *blobPump) kick() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

func (p *blobPump) run() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.trigger:
		}
		p.cycle()
	}
}

// cycle runs one observation and parks the result. A failed cycle parks nothing:
// the round that finds no snapshot emits an observation without stream values.
func (p *blobPump) cycle() {
	p.mu.Lock()
	in := p.input
	p.mu.Unlock()

	if !p.enabled() || in.seqNr == 0 || len(in.streams) == 0 || in.lifeCycleStage == protocol.LifeCycleStageRetired {
		return
	}

	p.inFlight.Store(true)
	defer p.inFlight.Store(false)

	snap, err := p.observe(in)
	if err != nil {
		p.lggr.Warnw("Blob pump cycle failed; round will observe no stream values", "err", err, "seqNr", in.seqNr, "streams", len(in.streams))
		return
	}

	p.cycles.Add(1)
	p.mu.Lock()
	p.ready = snap
	p.mu.Unlock()

	if p.verboseLogging {
		p.lggr.Debugw("Blob pump parked snapshot", "seqNr", in.seqNr, "expiresAt", snap.expiresAt, "streams", snap.streamCount, "handleBytes", len(snap.handleBytes))
	}
}

func (p *blobPump) observe(in pumpInput) (*blobSnapshot, error) {
	sv := make(protocol.StreamValues, len(in.streams))
	for _, sid := range in.streams {
		sv[sid] = nil
	}

	ctx, cancel := context.WithTimeout(p.ctx, p.observationTimeout)
	defer cancel()

	observedAt := time.Now()
	opts := datasource.NewDSOpts(p.verboseLogging, in.seqNr, p.configDigest, observedAt, in.lifeCycleStage)
	if err := p.ds.Observe(ctx, sv, opts); err != nil {
		return nil, fmt.Errorf("DataSource.Observe error: %w", err)
	}

	payload, err := marshalStreamValues(sv)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("no stream values observed for %d streams", len(in.streams))
	}

	expiresAt := in.seqNr + p.blobLifetimeRounds
	handle, err := p.bbf.BroadcastBlob(ctx, payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: expiresAt})
	if err != nil {
		return nil, fmt.Errorf("BroadcastBlob error: %w", err)
	}
	handleBytes, err := handle.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal blob handle: %w", err)
	}

	return &blobSnapshot{
		handleBytes: handleBytes,
		observedAt:  observedAt,
		forSeqNr:    in.seqNr,
		expiresAt:   expiresAt,
		streamCount: len(sv),
	}, nil
}

// marshalStreamValues serializes stream values into the stream-values-only
// proto that is carried by a blob, framed by encodeBlobPayload (which
// compresses it when that shrinks the payload). Returns nil when nothing was
// observed.
func marshalStreamValues(sv protocol.StreamValues) ([]byte, error) {
	pb, err := streamValuesToProto(sv)
	if err != nil {
		return nil, err
	}
	if len(pb) == 0 {
		return nil, nil
	}
	raw, err := proto.Marshal(&protocol.LLOObservationProto{StreamValues: pb})
	if err != nil {
		return nil, fmt.Errorf("marshal stream values: %w", err)
	}
	return encodeBlobPayload(raw)
}
