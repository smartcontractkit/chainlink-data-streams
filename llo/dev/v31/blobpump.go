package llo

import (
	"context"
	"fmt"
	"runtime/debug"
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
	// DefaultMaxSnapshotRounds bounds freshness and is enforced LOCALLY, by this
	// node's Take: it decides how stale the parked stream values may be when this
	// oracle references them in an observation. It has no effect on the blob
	// transport. Shortening it makes this node discard its own snapshot sooner,
	// it does not make the blob unfetchable for anyone. A snapshot gathered for
	// seqNr N is usable through N+MaxSnapshotRounds-1; the default of 2 means
	// consume at N+1, tolerating one skipped round. Tune against the report
	// format's staleness budget.
	DefaultMaxSnapshotRounds = 2
	// DefaultBlobLifetimeRounds bounds fetchability and is enforced REMOTELY, by
	// libocr's blob transport: it is the expiration hint passed to BroadcastBlob,
	// after which peers can no longer fetch the blob and the handle in an
	// observation resolves to nothing. It does not bound staleness: a node's own
	// freshness gate is DefaultMaxSnapshotRounds. Tune against network fetch
	// latency and reaping lag, not against data freshness.
	DefaultBlobLifetimeRounds = 4
	// BlobFetchMarginRounds couples the two: the number of rounds that must remain
	// between the last seqNr at which this node may reference a snapshot (local,
	// MaxSnapshotRounds) and the seqNr at which its blob expires for peers
	// (remote, BlobLifetimeRounds). Guarantees a handle that is still locally
	// usable is still remotely fetchable, with slack for slow or lagging peers.
	BlobFetchMarginRounds = 3
	// DefaultBlobObservationDurationMultiplier scales MaxDurationObservation
	// into the pump's per-cycle budget. The pump runs off the OCR critical
	// path, so it can afford to wait longer than a synchronous observation.
	DefaultBlobObservationDurationMultiplier = 2
	// SnapshotAgeSlack scales the measured round period into the wall-clock age
	// at which a parked snapshot is discarded. The bound is derived from the
	// observed round period rather than from MaxDurationObservation, which is
	// unrelated to the round cadence: a bound shorter than one round period
	// would reject every snapshot and silently stop the node contributing
	// stream values. Slack makes this a jitter guard, not a second freshness
	// gate, since staleness in rounds is already bounded by MaxSnapshotRounds.
	SnapshotAgeSlack = 2
	// MaxBlobLifetimeRounds bounds BlobLifetimeRounds. The pump broadcasts
	// roughly one blob per round, so the unexpired-blob budget declared to
	// libocr grows with the lifetime; this keeps that budget sane.
	MaxBlobLifetimeRounds = 64
	// MissStreakLogThreshold is how many consecutive rounds may find no usable
	// snapshot before the pump escalates from debug to error logging. A node
	// that never contributes stream values is a silent failure otherwise.
	MissStreakLogThreshold = 5
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
	// usableBefore is the local freshness gate: this node must not reference the
	// snapshot at or beyond this sequence number. Purely local, not on the wire.
	usableBefore uint64
	// expiresAt is the expiration hint given to the blob transport: peers cannot
	// fetch the blob at or beyond this sequence number. Always greater than
	// usableBefore by at least BlobFetchMarginRounds.
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
	blobPumpParams
	lggr logger.Logger

	trigger chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	inFlight   atomic.Bool
	misses     atomic.Uint64
	cycles     atomic.Uint64
	missStreak atomic.Uint64

	mu    sync.Mutex
	input pumpInput
	ready *blobSnapshot
	// lastTakeAt and roundPeriod estimate the round cadence from the interval
	// between consecutive Take calls (one per round), which is the only signal
	// the plugin has: deltaRound is not part of the reporting plugin config.
	lastTakeAt  time.Time
	roundPeriod time.Duration
}

// blobPumpParams is the pump's configuration, resolved by the factory.
type blobPumpParams struct {
	bbf                ocr3_1types.BlobBroadcastFetcher
	ds                 DataSource
	configDigest       ocrtypes.ConfigDigest
	verboseLogging     bool
	observationTimeout time.Duration
	// maxSnapshotAge is the wall-clock jitter guard. Positive pins it to an
	// explicit duration, zero derives it from the measured round period, and
	// negative disables it, leaving maxSnapshotRounds as the only bound.
	maxSnapshotAge time.Duration
	// maxSnapshotRounds is the local freshness gate. See DefaultMaxSnapshotRounds.
	maxSnapshotRounds uint64
	// blobLifetimeRounds is the remote fetchability bound. See DefaultBlobLifetimeRounds.
	blobLifetimeRounds uint64
}

func newBlobPump(lggr logger.Logger, params blobPumpParams) *blobPump {
	ctx, cancel := context.WithCancel(context.Background())
	return &blobPump{
		blobPumpParams: params,
		lggr:           logger.Sugared(lggr).Named("BlobPump"),
		trigger:        make(chan struct{}, 1),
		ctx:            ctx,
		cancel:         cancel,
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
		p.miss()
		return nil, "blob pump disabled"
	}

	now := time.Now()

	p.mu.Lock()
	snap := p.ready
	p.ready = nil
	// Measure before resolving the limit, both under the same lock. On the very
	// first Take there is no previous call to measure against, so roundPeriod is
	// still zero and the derived age check is inert. That Take is also the one
	// that kicks the first cycle, though, so nothing is parked yet and the round
	// misses on "no snapshot parked" regardless. By the second Take, which is
	// the first that can see a snapshot, the gap has been measured and the check
	// is live. There is no round in which a snapshot exists and roundPeriod is
	// still zero.
	p.recordRoundLocked(now)
	ageLimit := p.snapshotAgeLimitLocked()
	p.mu.Unlock()

	defer p.kick()

	switch {
	case snap == nil:
		p.miss()
		if p.inFlight.Load() {
			return nil, "cycle in flight"
		}
		return nil, "no snapshot parked"
	case seqNr >= snap.usableBefore:
		p.miss()
		return nil, fmt.Sprintf("snapshot too stale (forSeqNr=%d usableBefore=%d expiresAt=%d)", snap.forSeqNr, snap.usableBefore, snap.expiresAt)
	case ageLimit > 0 && now.Sub(snap.observedAt) > ageLimit:
		p.miss()
		return nil, fmt.Sprintf("snapshot too old (age=%s max=%s)", now.Sub(snap.observedAt), ageLimit)
	default:
		p.missStreak.Store(0)
		return snap, ""
	}
}

// miss records a round that found no usable snapshot. Sustained misses means
// this node is not contributing at all. Record misses and log when above MissStreakLogThreshold.
func (p *blobPump) miss() {
	p.misses.Add(1)
	if streak := p.missStreak.Add(1); streak >= MissStreakLogThreshold && streak%MissStreakLogThreshold == 0 {
		p.lggr.Errorw("Blob pump has found no usable snapshot for consecutive rounds; this node is contributing no stream values",
			"missStreak", streak, "misses", p.misses.Load(), "cycles", p.cycles.Load(), "maxSnapshotAge", p.maxSnapshotAge, "maxSnapshotRounds", p.maxSnapshotRounds)
	}
}

// recordRoundLocked folds the interval since the previous Take into the round
// period estimate. Take is called once per round, so consecutive calls measure
// the round cadence.
//
// Rounds that observe no streams skip Take entirely, so the next gap spans several
// round periods and overestimates; a stall inflates one gap badly, and estimation
// takes about four rounds to decay it.
// Both leave the age bound too generous for a while rather than too tight,
// which is the direction that cannot silently stop this node contributing.
func (p *blobPump) recordRoundLocked(now time.Time) {
	if !p.lastTakeAt.IsZero() {
		gap := now.Sub(p.lastTakeAt)
		if p.roundPeriod == 0 {
			p.roundPeriod = gap
		} else {
			p.roundPeriod = (3*p.roundPeriod + gap) / 4
		}
	}
	p.lastTakeAt = now
}

// snapshotAgeLimitLocked resolves the wall-clock bound for this round. Zero
// means no bound at all, and is the fallback whenever the cadence is unknown:
// an unmeasured round period falls back to maxSnapshotRounds as the only
// staleness bound rather than to an invented duration. Rejecting every snapshot
// is the worse failure, because it stops the node contributing stream values
// while every round still succeeds, so it shows up as a counter and nothing
// else.
func (p *blobPump) snapshotAgeLimitLocked() time.Duration {
	switch {
	case p.maxSnapshotAge < 0:
		return 0
	case p.maxSnapshotAge > 0:
		return p.maxSnapshotAge
	case p.roundPeriod > 0:
		return time.Duration(p.maxSnapshotRounds) * p.roundPeriod * SnapshotAgeSlack
	default:
		return 0
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
		p.safeCycle()
	}
}

// safeCycle isolates a pump cycle from panics. The pump runs on its own
// goroutine off the OCR critical path, so a panic in the DataSource (for
// example on a malformed observation input) would otherwise take down the
// whole process. A panicking cycle parks nothing, exactly like a failed one.
func (p *blobPump) safeCycle() {
	defer func() {
		if r := recover(); r != nil {
			p.lggr.Errorw("Blob pump cycle panicked; round will observe no stream values", "panic", r, "stacktrace", string(debug.Stack()))
		}
	}()
	p.cycle()
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
		p.lggr.Debugw("Blob pump parked snapshot", "seqNr", in.seqNr, "usableBefore", snap.usableBefore, "expiresAt", snap.expiresAt, "streams", snap.streamCount, "handleBytes", len(snap.handleBytes))
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

	usableBefore := in.seqNr + p.maxSnapshotRounds
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
		handleBytes:  handleBytes,
		observedAt:   observedAt,
		forSeqNr:     in.seqNr,
		usableBefore: usableBefore,
		expiresAt:    expiresAt,
		streamCount:  len(sv),
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
