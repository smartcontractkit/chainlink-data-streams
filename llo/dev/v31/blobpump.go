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
)

// Defaults for the blob pump. See PluginFactoryParams for the overrides.
const (
	// BlobFetchMarginRounds is how many rounds a blob must stay fetchable past
	// the round that references it, so slow or lagging peers can still fetch
	// it. Take offers a snapshot only while its blob expires at least this many
	// rounds after the current one.
	BlobFetchMarginRounds = 3
	// DefaultBlobLifetimeRounds is the expiration hint passed to BroadcastBlob,
	// in rounds after the cycle that produced the blob started. After it peers
	// can no longer fetch the blob and the handle in an observation resolves to
	// nothing. A snapshot can be offered for BlobLifetimeRounds minus
	// BlobFetchMarginRounds rounds, so the default leaves room for
	// SnapshotReuseRounds. It does not bound staleness: the state transition
	// does, from replicated data (see freshness.go).
	DefaultBlobLifetimeRounds = BlobFetchMarginRounds + SnapshotReuseRounds
	// MinBlobLifetimeRounds is the smallest lifetime that lets a snapshot be
	// offered for one round.
	MinBlobLifetimeRounds = BlobFetchMarginRounds + 1
	// DefaultBlobObservationDurationMultiplier scales MaxDurationObservation
	// into the pump's per-cycle budget. The pump runs off the OCR critical
	// path, so it can afford to wait longer than a synchronous observation.
	DefaultBlobObservationDurationMultiplier = 2
	// MaxBlobLifetimeRounds bounds BlobLifetimeRounds. The pump broadcasts
	// roughly one blob per round, so the unexpired-blob budget declared to
	// libocr grows with the lifetime; this keeps that budget sane.
	MaxBlobLifetimeRounds = 64
	// BlobBroadcastAttempts is how many times one cycle tries to broadcast the
	// payload it gathered.
	BlobBroadcastAttempts = 3
	// BlobBroadcastRetryBackoff is the wait before the second broadcast attempt,
	// doubled for each attempt after it. All attempts share the cycle's
	// observation timeout, so this cannot extend how long a cycle runs.
	BlobBroadcastRetryBackoff = 50 * time.Millisecond
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
	// closeTimeoutSlackMultiplier scales the pump's observation timeout into how
	// long Close waits for an in-flight cycle.
	closeTimeoutSlackMultiplier = 5
	// minCloseTimeout fixes the minimum time Close waits waits for an in-flight cycle.
	minCloseTimeout = 1 * time.Second
	// defaultBlobInFlightWaitFactor scales MaxDurationObservation into how long
	// Take waits for a cycle that is already in flight to park. Overridden by
	// PluginFactoryParams.BlobInFlightWaitFactor.
	defaultBlobInFlightWaitFactor = 8
)

// perOracleUnexpiredBlobCount derives the per-oracle unexpired-blob budget from
// the configured blob lifetime: one blob per round, plus a reaping margin.
func perOracleUnexpiredBlobCount(blobLifetimeRounds uint64) int {
	n := int(blobLifetimeRounds) + BlobReapingMarginRounds
	return max(n, MinPerOracleUnexpiredBlobCount)
}

// ResolveBlobLifetimeRounds applies the default to the blob lifetime knob and
// bounds it to [MinBlobLifetimeRounds, MaxBlobLifetimeRounds]. Shared with
// plugin config validation so a job spec is rejected at create time rather
// than at plugin startup.
func ResolveBlobLifetimeRounds(blobLifetimeRounds uint64) (uint64, error) {
	if blobLifetimeRounds == 0 {
		blobLifetimeRounds = DefaultBlobLifetimeRounds
	}
	if blobLifetimeRounds < MinBlobLifetimeRounds {
		return 0, fmt.Errorf("BlobLifetimeRounds (%d) is below MinBlobLifetimeRounds (%d): a snapshot could never be offered with %d rounds of fetch margin", blobLifetimeRounds, MinBlobLifetimeRounds, BlobFetchMarginRounds)
	}
	if blobLifetimeRounds > MaxBlobLifetimeRounds {
		return 0, fmt.Errorf("BlobLifetimeRounds (%d) exceeds MaxBlobLifetimeRounds (%d)", blobLifetimeRounds, MaxBlobLifetimeRounds)
	}
	return blobLifetimeRounds, nil
}

// pumpInput is the round context the pump needs, published by Observation.
type pumpInput struct {
	streams        []llotypes.StreamID
	seqNr          uint64
	lifeCycleStage llotypes.LifeCycleStage
}

// observable reports whether a cycle fed this input can produce a snapshot.
// An unset input (no round has published one yet), a round with nothing to
// observe, and a retired instance all park nothing by design.
func (in pumpInput) observable() bool {
	return in.seqNr != 0 && len(in.streams) > 0 && in.lifeCycleStage != protocol.LifeCycleStageRetired
}

// blobSnapshot is one completed pump cycle: stream values already serialized,
// broadcast as a blob, and reduced to the marshaled handle that goes on the
// wire.
type blobSnapshot struct {
	blobHandleBytes []byte
	// cycleStartSeqNr is the sequence number known when the cycle started. It
	// travels in the blob payload, where the state transition reads it to
	// judge the values' age.
	cycleStartSeqNr uint64
	// blobExpirationHintSeqNr is the expiration hint given to the blob
	// transport: peers cannot fetch the blob at or beyond this sequence number.
	// Not derivable from cycleStartSeqNr, since a broadcast retry recomputes
	// the hint from the round current at that attempt.
	blobExpirationHintSeqNr uint64
	// requestedStreams and observedStreams are how many streams the cycle was
	// asked for and how many the blob carries a value for (for logging).
	requestedStreams int
	observedStreams  int
}

// blobPump gathers stream observations off the OCR critical path and broadcasts
// them as blobs, parking the resulting handle for Observation to pick up.
//
// Cadence is consumption-driven: a cycle is kicked whenever Observation takes
// (or discards) a snapshot, so the pump rate tracks the round rate without
// needing to know deltaRound, and no blob is broadcast that no round asked for.
// Cycles are serial, so there is never more than one DataSource.Observe in
// flight. There is deliberately no idle watchdog: a snapshot's fetchability is
// bounded by sequence number, so refreshing while no rounds are running would
// only pile up blobs.
//
// The latest parked snapshot is kept until a newer one parks, and may be
// referenced by several rounds: a cycle that runs longer than a round costs
// nothing as long as the previous values are still fetchable, instead of a
// round with no stream values. A newly parked snapshot always replaces the
// previous one, taken or not.
//
// The pump does not judge whether a snapshot's values are still fresh. That
// decision is the state transition's, taken from replicated data so every
// oracle keeps the same values (see freshness.go). Take checks one thing only:
// that peers will still be able to fetch the blob.
type blobPump struct {
	blobPumpParams
	lggr logger.Logger

	trigger      chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	closeTimeout time.Duration

	inFlight   atomic.Bool
	misses     atomic.Uint64
	cycles     atomic.Uint64
	reuses     atomic.Uint64
	missStreak atomic.Uint64

	mu    sync.Mutex
	input pumpInput
	// latest is the newest parked snapshot, nil until the first cycle parks.
	// referenced records whether a round has already taken it.
	latest     *blobSnapshot
	referenced bool
	// cycleEnded is closed when the in-flight cycle ends, parked or not, and
	// replaced for the next one. Take waits on it.
	cycleEnded chan struct{}
}

// blobPumpParams is the pump's configuration, resolved by the factory.
type blobPumpParams struct {
	bbf                ocr3_1types.BlobBroadcastFetcher
	ds                 DataSource
	configDigest       ocrtypes.ConfigDigest
	verboseLogging     bool
	observationTimeout time.Duration
	// blobLifetimeRounds is the fetchability bound. See DefaultBlobLifetimeRounds.
	blobLifetimeRounds uint64
	// inFlightWait bounds how long Take waits for an in-flight cycle to park.
	inFlightWait time.Duration
}

func newBlobPump(lggr logger.Logger, params blobPumpParams) *blobPump {
	ctx, cancel := context.WithCancel(context.Background())
	return &blobPump{
		blobPumpParams: params,
		lggr:           logger.Sugared(lggr).Named("BlobPump"),
		trigger:        make(chan struct{}, 1),
		cycleEnded:     make(chan struct{}),
		ctx:            ctx,
		cancel:         cancel,
		closeTimeout:   max(params.observationTimeout*closeTimeoutSlackMultiplier, minCloseTimeout),
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

// Close stops the pump and waits for any in-flight cycle to unwind, bounded by
// closeTimeout. Reports false when the cycle did not unwind, meaning its
// goroutine is still blocked in a DataSource or broadcaster call that ignored
// the cancelled context: abandon it rather than let it hang the caller's
// shutdown path.
func (p *blobPump) Close() bool {
	p.cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(p.closeTimeout):
		p.lggr.Errorw("Blob pump cycle did not unwind after context cancellation; abandoning goroutine",
			"closeTimeout", p.closeTimeout, "observationTimeout", p.observationTimeout)
		return false
	}
}

// SetInput publishes the round context for subsequent cycles. Cheap; called
// from Observation before Take.
func (p *blobPump) SetInput(in pumpInput) {
	p.mu.Lock()
	p.input = in
	p.mu.Unlock()
}

// Take returns the latest parked snapshot if peers can still fetch its blob at
// seqNr, and always kicks the next cycle: kicking on a discard as well as on a
// hit is what stops a single unusable snapshot from stalling the pump forever.
// The second return value is the reason a snapshot was not returned, for
// logging.
//
// A snapshot stays offered until a newer one parks, so a round may take what
// an earlier round already took. A round that finds nothing new while a cycle
// is running waits up to inFlightWait for the cycle to end, or until ctx is
// done, and then falls back to what is parked.
func (p *blobPump) Take(ctx context.Context, seqNr uint64) (*blobSnapshot, string) {
	if !p.enabled() {
		return nil, "blob pump disabled"
	}

	defer p.kick()
	snap, reused, waited := p.take(ctx, seqNr, p.inFlightWait)

	switch {
	case snap == nil && !p.observable():
		return nil, "nothing to observe this round"
	case snap == nil:
		p.miss()
		if waited || p.inFlight.Load() {
			return nil, "cycle in flight"
		}
		return nil, "no snapshot parked"
	case !snap.isFetchableAtSeqNr(seqNr):
		p.miss()
		return nil, fmt.Sprintf("snapshot blob expires too soon for peers to fetch (cycleStartSeqNr=%d blobExpirationHintSeqNr=%d seqNr=%d margin=%d)", snap.cycleStartSeqNr, snap.blobExpirationHintSeqNr, seqNr, BlobFetchMarginRounds)
	case !snap.isWithinRoundAgeAtSeqNr(seqNr):
		p.miss()
		return nil, fmt.Sprintf("snapshot is past the stream value age bound (cycleStartSeqNr=%d seqNr=%d maxAgeRounds=%d)", snap.cycleStartSeqNr, seqNr, MaxStreamValueAgeRounds)
	default:
		p.missStreak.Store(0)
		if reused {
			p.reuses.Add(1)
		}
		return snap, ""
	}
}

// observable reports whether the published input can produce a snapshot.
func (p *blobPump) observable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.observable()
}

// take returns the latest usable snapshot and marks it referenced, reporting
// whether an earlier round had already referenced it. When nothing new is
// parked and a cycle is in flight it first waits up to timeout for that cycle
// to end, early if ctx is done or the pump shuts down. A snapshot no round can
// reference any more counts as nothing new, so the wait also covers the
// handover from an expiring snapshot to the one in flight.
func (p *blobPump) take(ctx context.Context, seqNr uint64, timeout time.Duration) (snap *blobSnapshot, reused, waited bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	nothingNew := p.latest == nil || p.referenced || !p.latest.isUsableAtSeqNr(seqNr)
	if timeout > 0 && nothingNew && p.inFlight.Load() {
		waited = true
		ended := p.cycleEnded
		p.mu.Unlock()
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		select {
		case <-ended:
		case <-waitCtx.Done():
		case <-p.ctx.Done():
		}
		cancel()
		p.mu.Lock()
	}

	if p.latest == nil {
		return nil, false, waited
	}
	reused = p.referenced
	p.referenced = true
	return p.latest, reused, waited
}

// isFetchableAtSeqNr reports whether a round at seqNr may reference the
// snapshot: its blob must stay fetchable for BlobFetchMarginRounds more
// rounds. libocr expires a blob once the committed seqNr reaches the hint, so
// a round at seqNr can fetch a blob while seqNr <= hint. The margin covers
// peers that fetch late.
func (s *blobSnapshot) isFetchableAtSeqNr(seqNr uint64) bool {
	return seqNr <= s.blobExpirationHintSeqNr && s.blobExpirationHintSeqNr-seqNr >= BlobFetchMarginRounds
}

// isWithinRoundAgeAtSeqNr reports whether a round at seqNr would still pass the
// state transition's round age bound for the snapshot.
func (s *blobSnapshot) isWithinRoundAgeAtSeqNr(seqNr uint64) bool {
	return seqNr >= s.cycleStartSeqNr && seqNr-s.cycleStartSeqNr <= MaxStreamValueAgeRounds
}

// isUsableAtSeqNr reports whether a round at seqNr may reference the snapshot.
func (s *blobSnapshot) isUsableAtSeqNr(seqNr uint64) bool {
	return s.isFetchableAtSeqNr(seqNr) && s.isWithinRoundAgeAtSeqNr(seqNr)
}

// park makes a snapshot the one Take offers. The newest snapshot always wins,
// whether or not a round took the previous one.
func (p *blobPump) park(snap *blobSnapshot) {
	p.mu.Lock()
	p.latest = snap
	p.referenced = false
	p.mu.Unlock()
}

// beginCycle marks a cycle in flight; endCycle clears it and releases every
// Take waiting on it. Both run under mu so a Take never captures the channel of
// a cycle that has already ended.
func (p *blobPump) beginCycle() {
	p.mu.Lock()
	p.inFlight.Store(true)
	p.mu.Unlock()
}

func (p *blobPump) endCycle() {
	p.mu.Lock()
	p.inFlight.Store(false)
	close(p.cycleEnded)
	p.cycleEnded = make(chan struct{})
	p.mu.Unlock()
}

// miss records a round that found no usable snapshot when one was expected.
// Sustained misses means this node is not contributing at all. Record misses
// and log when above MissStreakLogThreshold.
func (p *blobPump) miss() {
	p.misses.Add(1)
	if streak := p.missStreak.Add(1); streak >= MissStreakLogThreshold && streak%MissStreakLogThreshold == 0 {
		p.lggr.Errorw("Blob pump has found no usable snapshot for consecutive rounds; this node is contributing no stream values",
			"missStreak", streak, "misses", p.misses.Load(), "cycles", p.cycles.Load(), "blobLifetimeRounds", p.blobLifetimeRounds)
	}
}

// Misses reports how many rounds found no usable snapshot.
func (p *blobPump) Misses() uint64 { return p.misses.Load() }

// Cycles reports how many snapshots the pump has successfully parked.
func (p *blobPump) Cycles() uint64 { return p.cycles.Load() }

// Reuses reports how many rounds took a snapshot an earlier round had already
// taken.
func (p *blobPump) Reuses() uint64 { return p.reuses.Load() }

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

// cycle runs one observation and parks the result. A failed cycle parks
// nothing: the previous snapshot stays offered while fetchable, and a round
// that finds none emits an observation without stream values.
func (p *blobPump) cycle() {
	p.mu.Lock()
	in := p.input
	p.mu.Unlock()

	if !p.enabled() || !in.observable() {
		return
	}

	p.beginCycle()
	defer p.endCycle()

	snap, err := p.observeStreamValuesAndBroadcastBlob(in)
	if err != nil {
		p.lggr.Warnw("Blob pump cycle failed; round will observe no stream values", "err", err, "seqNr", in.seqNr, "streams", len(in.streams))
		return
	}

	p.cycles.Add(1)
	p.park(snap)

	if p.verboseLogging {
		p.lggr.Debugw("Blob pump parked snapshot", "seqNr", in.seqNr, "cycleStartSeqNr", snap.cycleStartSeqNr, "blobExpirationHintSeqNr", snap.blobExpirationHintSeqNr, "requestedStreams", snap.requestedStreams, "observedStreams", snap.observedStreams, "handleBytes", len(snap.blobHandleBytes))
	}
}

func (p *blobPump) observeStreamValuesAndBroadcastBlob(in pumpInput) (*blobSnapshot, error) {
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
	// Broadcast only the requested streams. Peers reject an observation over
	// MaxObservationStreamValuesLength, so unrequested keys from the
	// DataSource could void this node's whole contribution.
	if len(sv) > len(in.streams) {
		requested := make(protocol.StreamValues, len(in.streams))
		for _, sid := range in.streams {
			requested[sid] = sv[sid]
		}
		p.lggr.Warnw("DataSource returned unrequested streams; dropping them", "seqNr", in.seqNr, "requested", len(in.streams), "returned", len(sv))
		sv = requested
	}
	dropUnobservable(sv, p.lggr, in.seqNr)
	observed := 0
	for _, v := range sv {
		if v != nil {
			observed++
		}
	}

	payload, err := marshalStreamValues(sv, in.seqNr)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("no stream values observed for %d streams", len(in.streams))
	}

	handle, expirationHintSeqNr, err := p.broadcast(ctx, payload, in.seqNr)
	if err != nil {
		return nil, err
	}
	handleBytes, err := handle.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal blob handle: %w", err)
	}

	return &blobSnapshot{
		blobHandleBytes:         handleBytes,
		cycleStartSeqNr:         in.seqNr,
		blobExpirationHintSeqNr: expirationHintSeqNr,
		requestedStreams:        len(in.streams),
		observedStreams:         observed,
	}, nil
}

// broadcast hands the payload to the blob transport, retrying a failed
// broadcast within the cycle's own timeout, and returns the handle together
// with the expiration hint it was broadcast under.
//
// cycleStartSeqNr is the round the values were gathered for. The expiration hint is
// recomputed per attempt from the latest round published by Observation, so a
// retry that lands rounds later does not hand peers a blob that expires as of
// a sequence number already behind them. The hint only moves forward, and it
// bounds fetchability, not staleness: how stale the values themselves may be is
// decided by the state transition from cycleStartSeqNr, which a retry does not
// change.
func (p *blobPump) broadcast(ctx context.Context, payload []byte, cycleStartSeqNr uint64) (ocr3_1types.BlobHandle, uint64, error) {
	var lastErr error
	for attempt := 1; attempt <= BlobBroadcastAttempts; attempt++ {
		expiresAt := max(cycleStartSeqNr, p.latestSeqNr()) + p.blobLifetimeRounds
		handle, err := p.bbf.BroadcastBlob(ctx, payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: expiresAt})
		if err == nil {
			if attempt > 1 {
				p.lggr.Infow("Blob broadcast succeeded after retry", "attempt", attempt, "cycleStartSeqNr", cycleStartSeqNr, "expiresAt", expiresAt)
			}
			return handle, expiresAt, nil
		}
		lastErr = err
		if attempt == BlobBroadcastAttempts {
			break
		}
		backoff := BlobBroadcastRetryBackoff << (attempt - 1)
		p.lggr.Warnw("Blob broadcast failed; retrying within this cycle", "attempt", attempt, "attempts", BlobBroadcastAttempts, "backoff", backoff, "cycleStartSeqNr", cycleStartSeqNr, "err", err)
		select {
		case <-ctx.Done():
			return ocr3_1types.BlobHandle{}, 0, fmt.Errorf("BroadcastBlob error: %w (last attempt: %w)", ctx.Err(), lastErr)
		case <-time.After(backoff):
		}
	}
	return ocr3_1types.BlobHandle{}, 0, fmt.Errorf("BroadcastBlob error after %d attempts: %w", BlobBroadcastAttempts, lastErr)
}

// latestSeqNr is the most recent round Observation published, which may be
// ahead of the round a cycle started for.
func (p *blobPump) latestSeqNr() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.seqNr
}

// dropUnobservable unsets every value peers would reject on decode, such as one
// over MaxDecimalCoefficientBits. Peers reject the whole observation on the
// first such value, so one bad value from the DataSource would otherwise void
// every other value this node contributes. The check is the receiver's own
// decode, so both sides apply the same rules.
func dropUnobservable(sv protocol.StreamValues, lggr logger.Logger, seqNr uint64) {
	for id, v := range sv {
		if v == nil {
			continue
		}
		pb, err := protocol.StreamValueToProto(v)
		if err == nil {
			_, err = streamValueFromProtoAllowNil(pb)
		}
		if err != nil {
			lggr.Warnw("Dropping stream value peers would reject", "streamID", id, "err", err, "seqNr", seqNr)
			sv[id] = nil
		}
	}
}

// marshalStreamValues serializes stream values and the seqNr the cycle that
// gathered them started in into the blob payload proto, framed by
// encodeBlobPayload (which compresses it when that shrinks the payload).
// Returns nil when nothing was observed.
func marshalStreamValues(sv protocol.StreamValues, cycleStartSeqNr uint64) ([]byte, error) {
	pb, err := streamValuesToProto(sv)
	if err != nil {
		return nil, err
	}
	if len(pb) == 0 {
		return nil, nil
	}

	raw, err := deterministicMarshal.Marshal(&protocol.LLOBlobPayloadProto{CycleStartSeqNr: cycleStartSeqNr, StreamValues: pb})
	if err != nil {
		return nil, fmt.Errorf("marshal stream values: %w", err)
	}
	return encodeBlobPayload(raw)
}
