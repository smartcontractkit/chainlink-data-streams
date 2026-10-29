package llo

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31/llotest"
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

func testPump(t *testing.T, ds DataSource, bbf ocr3_1types.BlobBroadcastFetcher) *blobPump {
	t.Helper()
	p := newBlobPump(logger.Test(t), blobPumpParams{
		bbf:                bbf,
		ds:                 ds,
		configDigest:       ocrtypes.ConfigDigest{1},
		verboseLogging:     true,
		observationTimeout: tests.WaitTimeout(t),
		blobLifetimeRounds: DefaultBlobLifetimeRounds,
	})
	p.Start()
	t.Cleanup(func() { assert.True(t, p.Close(), "pump did not stop cleanly") })
	return p
}

func pumpInputFor(seqNr uint64) pumpInput {
	return pumpInput{streams: []llotypes.StreamID{100}, seqNr: seqNr, lifeCycleStage: protocol.LifeCycleStageProduction}
}

func mockDS() *mockDataSource {
	return &mockDataSource{vals: protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(7))}}
}

// Test_blobPump_TakeKicksNextCycle covers the cadence contract: the first Take
// finds nothing and kicks a cycle, and the snapshot it produces is served to the
// next Take.
func Test_blobPump_TakeKicksNextCycle(t *testing.T) {
	ds := mockDS()
	bc := newFakeBroadcaster()
	p := testPump(t, ds, bc)

	p.SetInput(pumpInputFor(2))
	snap, reason := p.Take(t.Context(), 2)
	require.Nil(t, snap)
	require.NotEmpty(t, reason)

	require.Eventually(t, func() bool { return p.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)

	p.SetInput(pumpInputFor(3))
	snap, reason = p.Take(t.Context(), 3)
	require.NotNil(t, snap, "reason: %s", reason)
	require.Equal(t, uint64(2), snap.cycleStartSeqNr)
	require.Equal(t, uint64(2+DefaultBlobLifetimeRounds), snap.blobExpirationHintSeqNr)
	require.NotEmpty(t, snap.blobHandleBytes)
	require.Equal(t, uint64(1), p.Misses())

	// The blob was hinted to expire at cycleStartSeqNr + lifetime. Assert on the first
	// broadcast: later cycles re-broadcast an identical payload, which is
	// content-addressed to the same handle, so only the ordered hint log
	// distinguishes them.
	hints := bc.Hints()
	require.NotEmpty(t, hints)
	require.Equal(t, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: snap.blobExpirationHintSeqNr}, hints[0])

	// The snapshot's handle fetches back the payload the pump broadcast.
	var handle ocr3_1types.BlobHandle
	require.NoError(t, handle.UnmarshalBinary(snap.blobHandleBytes))
	payload, err := bc.FetchBlob(tests.Context(t), handle)
	require.NoError(t, err)
	require.NotEmpty(t, payload)

	// Taking a snapshot also kicks, so the pump keeps running.
	require.Eventually(t, func() bool { return p.Cycles() >= 2 }, tests.WaitTimeout(t), 10*time.Millisecond)
}

func Test_blobPump_DropsUnrequestedStreams(t *testing.T) {
	ds := mockDS()
	ds.vals[999] = protocol.ToDecimal(decimal.NewFromInt(9))
	p := testPump(t, ds, newFakeBroadcaster())

	p.SetInput(pumpInputFor(2))
	p.Take(t.Context(), 2)
	require.Eventually(t, func() bool { return p.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)

	p.SetInput(pumpInputFor(3))
	snap, reason := p.Take(t.Context(), 3)
	require.NotNil(t, snap, "reason: %s", reason)
	require.Equal(t, 1, snap.requestedStreams)
	require.Equal(t, 1, snap.observedStreams)
}

func Test_dropUnobservable(t *testing.T) {
	over := new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits+1)
	sv := protocol.StreamValues{
		1: protocol.ToDecimal(decimal.NewFromInt(7)),
		2: protocol.ToDecimal(decimal.NewFromBigInt(over, 0)),
		3: nil,
	}
	dropUnobservable(sv, logger.Test(t), 1)
	assert.Equal(t, protocol.ToDecimal(decimal.NewFromInt(7)), sv[1])
	assert.Nil(t, sv[2])
	assert.Nil(t, sv[3])

	// What is left decodes on the receiving side.
	payload, err := marshalStreamValues(sv, 1)
	require.NoError(t, err)
	raw, err := decodeBlobPayload(payload, maxDecompressedBlobPayloadBytes)
	require.NoError(t, err)
	chunk := &protocol.LLOBlobPayloadProto{}
	require.NoError(t, proto.Unmarshal(raw, chunk))
	require.Len(t, chunk.StreamValues, 1)
	_, err = streamValueFromProtoAllowNil(chunk.StreamValues[1])
	require.NoError(t, err)
}

// Test_blobPump_ReusesSnapshotUntilNewerParks covers reuse: the parked
// snapshot serves every round until a newer one replaces it, taken or not. The
// data source never returns, so only the snapshots parked by hand are offered.
func Test_blobPump_ReusesSnapshotUntilNewerParks(t *testing.T) {
	ds := &gatedDataSource{release: make(chan struct{}), entered: make(chan struct{})}
	defer close(ds.release)
	p := testPump(t, ds, newFakeBroadcaster())
	p.SetInput(pumpInputFor(2))

	first := &blobSnapshot{blobHandleBytes: []byte{1}, cycleStartSeqNr: 2, blobExpirationHintSeqNr: 2 + DefaultBlobLifetimeRounds}
	p.park(first)
	for seqNr := uint64(3); seqNr <= 2+SnapshotReuseRounds; seqNr++ {
		snap, reason := p.Take(t.Context(), seqNr)
		require.Same(t, first, snap, "seqNr %d: %s", seqNr, reason)
	}
	require.Equal(t, uint64(SnapshotReuseRounds-1), p.Reuses())
	require.Zero(t, p.Misses())

	// Past the fetch margin the old snapshot is withheld, not replaced.
	snap, reason := p.Take(t.Context(), 3+SnapshotReuseRounds)
	require.Nil(t, snap)
	require.Contains(t, reason, "expires too soon")
	require.Equal(t, uint64(1), p.Misses())

	// A newer snapshot replaces the previous one whether or not it was taken.
	second := &blobSnapshot{blobHandleBytes: []byte{2}, cycleStartSeqNr: 6, blobExpirationHintSeqNr: 6 + DefaultBlobLifetimeRounds}
	p.park(second)
	third := &blobSnapshot{blobHandleBytes: []byte{3}, cycleStartSeqNr: 7, blobExpirationHintSeqNr: 7 + DefaultBlobLifetimeRounds}
	p.park(third)
	snap, reason = p.Take(t.Context(), 8)
	require.Same(t, third, snap, reason)
	require.Equal(t, uint64(SnapshotReuseRounds-1), p.Reuses(), "first take of a new snapshot is not a reuse")
}

// Test_blobPump_TakeWaitsForNewerWhenReferenced asserts a round that already
// has the parked snapshot on the wire waits for the in-flight cycle, so a
// newer snapshot is preferred to a reuse whenever one lands in time.
func Test_blobPump_TakeWaitsForNewerWhenReferenced(t *testing.T) {
	ds := &gatedDataSource{release: make(chan struct{}), entered: make(chan struct{})}
	p := testPump(t, ds, newFakeBroadcaster())
	p.inFlightWait = tests.WaitTimeout(t)
	p.SetInput(pumpInputFor(2))

	old := &blobSnapshot{blobHandleBytes: []byte{1}, cycleStartSeqNr: 2, blobExpirationHintSeqNr: 2 + DefaultBlobLifetimeRounds}
	p.park(old)
	snap, _ := p.Take(t.Context(), 3)
	require.Same(t, old, snap)
	select {
	case <-ds.entered:
	case <-time.After(tests.WaitTimeout(t)):
		t.Fatal("DataSource.Observe was never called")
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(ds.release)
	}()
	snap, reason := p.Take(t.Context(), 4)
	require.NotNil(t, snap, reason)
	require.NotSame(t, old, snap, "Take reused the old snapshot instead of waiting for the cycle")
	require.Zero(t, p.Reuses())
}

func Test_blobPump_OffersSnapshotWhileFetchable(t *testing.T) {
	// The only local gate: the blob must stay fetchable for
	// BlobFetchMarginRounds past the round that references it. Freshness is
	// the state transition's call.
	park := func(p *blobPump, hint uint64) {
		p.park(&blobSnapshot{blobHandleBytes: []byte{1}, cycleStartSeqNr: 2, blobExpirationHintSeqNr: hint})
	}

	t.Run("offered up to the margin", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster())
		for seqNr := uint64(3); seqNr+BlobFetchMarginRounds <= 2+DefaultBlobLifetimeRounds; seqNr++ {
			park(p, 2+DefaultBlobLifetimeRounds)
			snap, reason := p.Take(t.Context(), seqNr)
			require.NotNil(t, snap, "seqNr %d: %s", seqNr, reason)
		}
		require.Zero(t, p.Misses())
	})

	t.Run("withheld inside the margin", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster())
		park(p, 2+DefaultBlobLifetimeRounds)
		snap, reason := p.Take(t.Context(), 2+DefaultBlobLifetimeRounds-BlobFetchMarginRounds+1)
		require.Nil(t, snap)
		require.Contains(t, reason, "expires too soon")
		require.Equal(t, uint64(1), p.Misses())
	})

	t.Run("withheld past expiry", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster())
		park(p, 6)
		snap, reason := p.Take(t.Context(), 7)
		require.Nil(t, snap)
		require.Contains(t, reason, "expires too soon")
	})

	t.Run("withheld past the round age bound even while fetchable", func(t *testing.T) {
		// A long lifetime, or a broadcast retry that landed rounds late, keeps
		// the blob fetchable past the age bound the state transition applies.
		p := testPump(t, mockDS(), newFakeBroadcaster())
		park(p, 2+MaxBlobLifetimeRounds)
		snap, reason := p.Take(t.Context(), 2+MaxStreamValueAgeRounds)
		require.NotNil(t, snap, reason)
		park(p, 2+MaxBlobLifetimeRounds)
		snap, reason = p.Take(t.Context(), 2+MaxStreamValueAgeRounds+1)
		require.Nil(t, snap)
		require.Contains(t, reason, "past the stream value age bound")
		require.Equal(t, uint64(1), p.Misses())
	})

	t.Run("the default lifetime covers the reuse horizon", func(t *testing.T) {
		// A snapshot gathered during round N is first offered at N+1 and must
		// still be offered at N+SnapshotReuseRounds.
		snap := &blobSnapshot{cycleStartSeqNr: 2, blobExpirationHintSeqNr: 2 + DefaultBlobLifetimeRounds}
		require.True(t, snap.isFetchableAtSeqNr(2+SnapshotReuseRounds))
		require.False(t, snap.isFetchableAtSeqNr(2+SnapshotReuseRounds+1))
	})
}

// Test_blobPump_ParksNothingOnFailure covers both failure modes: a data-source
// error and a broadcast error. Neither may park a snapshot, since stream values
// are only ever disseminated by blob.
func Test_blobPump_ParksNothingOnFailure(t *testing.T) {
	t.Run("data source error", func(t *testing.T) {
		ds := mockDS()
		ds.err = errors.New("bridge down")
		p := testPump(t, ds, newFakeBroadcaster())
		p.SetInput(pumpInputFor(2))
		_, _ = p.Take(t.Context(), 2)

		require.Eventually(t, func() bool { return ds.observeCount() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
		require.Zero(t, p.Cycles())
		snap, _ := p.Take(t.Context(), 3)
		require.Nil(t, snap)
	})

	t.Run("broadcast error", func(t *testing.T) {
		bc := func() *llotest.BlobBroadcastFetcher {
			bc := newFakeBroadcaster()
			bc.SetBroadcastError(errors.New("broadcast unavailable"))
			return bc
		}()
		p := testPump(t, mockDS(), bc)
		p.SetInput(pumpInputFor(2))
		_, _ = p.Take(t.Context(), 2)

		require.Eventually(t, func() bool { return bc.Broadcasts() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
		require.Zero(t, p.Cycles())
		snap, _ := p.Take(t.Context(), 3)
		require.Nil(t, snap)
	})
}

// Test_blobPump_SkipsIdleInput asserts the pump does not observe (or spend blob
// budget) when there is nothing to observe.
func Test_blobPump_SkipsIdleInput(t *testing.T) {
	for name, in := range map[string]pumpInput{
		"no input yet": {},
		"no streams":   {seqNr: 2, lifeCycleStage: protocol.LifeCycleStageProduction},
		"retired":      {streams: []llotypes.StreamID{100}, seqNr: 2, lifeCycleStage: protocol.LifeCycleStageRetired},
	} {
		t.Run(name, func(t *testing.T) {
			ds := mockDS()
			bc := newFakeBroadcaster()
			p := testPump(t, ds, bc)
			p.SetInput(in)
			for i := 0; i < 3; i++ {
				_, _ = p.Take(t.Context(), 2)
			}
			// Give the loop a chance to run the kicked cycles.
			require.Never(t, func() bool { return ds.observeCount() > 0 || bc.Broadcasts() > 0 }, 100*time.Millisecond, 10*time.Millisecond)
			require.Zero(t, p.Cycles())
		})
	}
}

// Test_blobPump_SingleFlight asserts cycles are serial: however many kicks
// arrive, only one DataSource.Observe runs at a time.
func Test_blobPump_SingleFlight(t *testing.T) {
	release := make(chan struct{})
	ds := &blockingDataSource{release: release}
	p := testPump(t, ds, newFakeBroadcaster())
	p.SetInput(pumpInputFor(2))

	for i := 0; i < 10; i++ {
		_, _ = p.Take(t.Context(), 2)
	}
	require.Eventually(t, func() bool { return ds.started() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Never(t, func() bool { return ds.concurrent() > 1 }, 100*time.Millisecond, 10*time.Millisecond)
	close(release)
}

// gatedDataSource blocks until released and then observes normally, modelling a
// cycle that is still gathering values when Take arrives.
type gatedDataSource struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
	// err, when set, fails the observation instead of producing values, so the
	// cycle unwinds without parking anything.
	err error
}

func (g *gatedDataSource) Observe(ctx context.Context, sv protocol.StreamValues, opts DSOpts) error {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if g.err != nil {
		return g.err
	}
	sv[100] = protocol.ToDecimal(decimal.NewFromInt(7))
	return nil
}

// Test_blobPump_TakeWaitsForInFlightCycle covers the rescue path: a Take that
// finds nothing parked while a cycle is gathering waits for that cycle instead
// of missing the round outright. The data source is released only once the
// second Take is already waiting, so the snapshot can only have been served by
// the wait.
func Test_blobPump_TakeWaitsForInFlightCycle(t *testing.T) {
	ds := &gatedDataSource{release: make(chan struct{}), entered: make(chan struct{})}
	p := testPump(t, ds, newFakeBroadcaster())
	p.inFlightWait = tests.WaitTimeout(t)

	// The first Take only kicks the cycle: nothing is in flight yet, so there is
	// nothing for it to wait on.
	p.SetInput(pumpInputFor(2))
	snap, reason := p.Take(t.Context(), 2)
	require.Nil(t, snap)
	require.Equal(t, "no snapshot parked", reason)

	select {
	case <-ds.entered:
	case <-time.After(tests.WaitTimeout(t)):
		t.Fatal("DataSource.Observe was never called")
	}
	require.True(t, p.inFlight.Load())

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(ds.release)
	}()

	snap, reason = p.Take(t.Context(), 2)
	require.NotNil(t, snap, "Take did not wait for the in-flight cycle: %s", reason)
	require.Empty(t, reason)
}

// Test_blobPump_TakeWaitFallsThroughOnTimeout asserts the wait is bounded and
// the miss path stays the fallback: a cycle that does not park in time still
// misses the round rather than holding up the observation.
func Test_blobPump_TakeWaitFallsThroughOnTimeout(t *testing.T) {
	ds := &gatedDataSource{release: make(chan struct{}), entered: make(chan struct{})}
	defer close(ds.release)

	p := testPump(t, ds, newFakeBroadcaster())
	p.inFlightWait = 50 * time.Millisecond

	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(t.Context(), 2)
	select {
	case <-ds.entered:
	case <-time.After(tests.WaitTimeout(t)):
		t.Fatal("DataSource.Observe was never called")
	}

	start := time.Now()
	snap, reason := p.Take(t.Context(), 2)
	elapsed := time.Since(start)
	require.Nil(t, snap)
	require.Equal(t, "cycle in flight", reason)
	require.GreaterOrEqual(t, elapsed, p.inFlightWait, "Take returned before the wait elapsed")
	require.Less(t, elapsed, 10*p.inFlightWait, "Take waited well past its bound")
}

// Test_blobPump_RoundPeriodOnlyMeasuresConsecutiveRounds pins the cadence
// estimate to gaps that really are one round wide: rounds that observe no
// streams skip Take, and folding the wider gap they leave would inflate the
// derived age bound.
// Test_blobPump_ExpectedMissesAreNotCounted covers the rounds that lose their
// stream values by design: a pump with no transport wired, and a round whose
// input gives a cycle nothing to do. Neither is a miss, so the counters stay
// clean and the streak error stays reserved for a pump that should be
// producing and is not.
func Test_blobPump_ExpectedMissesAreNotCounted(t *testing.T) {
	t.Run("disabled pump", func(t *testing.T) {
		p := testPump(t, nil, nil)

		for i := 0; i < 2*MissStreakLogThreshold; i++ {
			snap, reason := p.Take(t.Context(), 2)
			require.Nil(t, snap)
			require.Equal(t, "blob pump disabled", reason)
		}
		require.Zero(t, p.Misses())
		require.Zero(t, p.missStreak.Load())
	})

	t.Run("nothing to observe", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster())
		p.SetInput(pumpInput{streams: nil, seqNr: 2, lifeCycleStage: protocol.LifeCycleStageProduction})

		snap, reason := p.Take(t.Context(), 2)
		require.Nil(t, snap)
		require.Equal(t, "nothing to observe this round", reason)
		require.Zero(t, p.Misses())
		require.Zero(t, p.missStreak.Load())
	})

	t.Run("retired", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster())
		p.SetInput(pumpInput{streams: []llotypes.StreamID{100}, seqNr: 2, lifeCycleStage: protocol.LifeCycleStageRetired})

		snap, reason := p.Take(t.Context(), 2)
		require.Nil(t, snap)
		require.Equal(t, "nothing to observe this round", reason)
		require.Zero(t, p.Misses())
		require.Zero(t, p.missStreak.Load())
	})
}

// Test_blobPump_TakeWaitHonoursContext asserts a canceled round context ends
// the in-flight wait early: OCR3.1 does not bound Observation, so cancellation
// is the only thing that can cut this wait short.
func Test_blobPump_TakeWaitHonoursContext(t *testing.T) {
	ds := &gatedDataSource{release: make(chan struct{}), entered: make(chan struct{})}
	defer close(ds.release)

	p := testPump(t, ds, newFakeBroadcaster())
	p.inFlightWait = tests.WaitTimeout(t)

	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(t.Context(), 2)
	select {
	case <-ds.entered:
	case <-time.After(tests.WaitTimeout(t)):
		t.Fatal("DataSource.Observe was never called")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	snap, reason := p.Take(ctx, 2)
	elapsed := time.Since(start)
	require.Nil(t, snap)
	require.Equal(t, "cycle in flight", reason)
	require.Less(t, elapsed, p.inFlightWait/2, "Take ignored the canceled context")
}

// Test_blobPump_TakeWaitReportsCycleAfterItEnds pins the miss reason for a
// round that waited: the cycle it waited on can finish empty and clear the
// in-flight flag before the wait expires, and the round still belongs to that
// cycle rather than to an absent snapshot.
func Test_blobPump_TakeWaitReportsCycleAfterItEnds(t *testing.T) {
	ds := &gatedDataSource{release: make(chan struct{}), entered: make(chan struct{}), err: errors.New("boom")}
	p := testPump(t, ds, newFakeBroadcaster())
	p.inFlightWait = 500 * time.Millisecond

	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(t.Context(), 2)
	select {
	case <-ds.entered:
	case <-time.After(tests.WaitTimeout(t)):
		t.Fatal("DataSource.Observe was never called")
	}

	// Fail the cycle while the next Take is waiting on it.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(ds.release)
	}()

	// The reason is resolved inside Take, before its deferred kick starts the
	// next cycle, so it is the only assertable evidence here: reading inFlight
	// after Take returns would race that new cycle.
	snap, reason := p.Take(t.Context(), 2)
	require.Nil(t, snap)
	require.Equal(t, "cycle in flight", reason)
}

func Test_observableStreams(t *testing.T) {
	state := &kvState{channelDefinitions: llotypes.ChannelDefinitions{
		1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{
			{StreamID: 100, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 101, Aggregator: llotypes.AggregatorCalculated},
		}},
		// Duplicate stream across channels must be listed once.
		2: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
		3: {ReportFormat: llotypes.ReportFormatJSON, Tombstone: true, Streams: []llotypes.Stream{{StreamID: 102, Aggregator: llotypes.AggregatorMedian}}},
		// Backfill reports are built from the channel opts, so its streams are
		// not observed. Here the target (3) is tombstoned, which is the case
		// where the backfill channel would otherwise keep 102 observed.
		4: {ReportFormat: llotypes.ReportFormatHistoryBackfill, Streams: []llotypes.Stream{{StreamID: 102, Aggregator: llotypes.AggregatorMedian}}},
	}}
	require.ElementsMatch(t, []llotypes.StreamID{100}, observableStreams(state))
	require.Empty(t, observableStreams(&kvState{}))
}

// Test_blobPump_DisabledIsInert covers hosts that run the plugin without blob
// transport (a nil BlobBroadcastFetcher, as some harnesses pass): the pump must
// stay inert instead of dereferencing the nil dependency in its loop goroutine.
func Test_blobPump_DisabledIsInert(t *testing.T) {
	t.Run("nil broadcaster", func(t *testing.T) {
		ds := mockDS()
		p := testPump(t, ds, nil)
		p.SetInput(pumpInputFor(2))
		for i := 0; i < 3; i++ {
			snap, reason := p.Take(t.Context(), 2)
			require.Nil(t, snap)
			require.Equal(t, "blob pump disabled", reason)
		}
		require.Never(t, func() bool { return ds.observeCount() > 0 }, 100*time.Millisecond, 10*time.Millisecond)
		require.Zero(t, p.Cycles())
	})

	t.Run("nil data source", func(t *testing.T) {
		bc := newFakeBroadcaster()
		p := testPump(t, nil, bc)
		p.SetInput(pumpInputFor(2))
		snap, reason := p.Take(t.Context(), 2)
		require.Nil(t, snap)
		require.Equal(t, "blob pump disabled", reason)
		require.Zero(t, bc.Broadcasts())
	})
}

type panicDataSource struct{ calls atomic.Int64 }

func (p *panicDataSource) Observe(ctx context.Context, sv protocol.StreamValues, opts DSOpts) error {
	p.calls.Add(1)
	panic("malformed observation input")
}

// Test_blobPump_SurvivesDataSourcePanic asserts a panicking DataSource does not
// take down the pump goroutine (and with it the process); the cycle simply parks
// nothing and later cycles still run.
func Test_blobPump_SurvivesDataSourcePanic(t *testing.T) {
	ds := &panicDataSource{}
	p := testPump(t, ds, newFakeBroadcaster())

	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(t.Context(), 2)
	require.Eventually(t, func() bool { return ds.calls.Load() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Zero(t, p.Cycles())

	// Pump goroutine is still alive: a second kick still reaches the DataSource.
	p.SetInput(pumpInputFor(3))
	_, _ = p.Take(t.Context(), 3)
	require.Eventually(t, func() bool { return ds.calls.Load() >= 2 }, tests.WaitTimeout(t), 10*time.Millisecond)
	// Take kicks before it returns, so the last kicked cycle may still be
	// running; it must unwind rather than leave the flag stuck.
	require.Eventually(t, func() bool { return !p.inFlight.Load() }, tests.WaitTimeout(t), 10*time.Millisecond)
}

// stuckDataSource ignores its context and blocks until released, modelling a
// host DataSource that does not honor cancellation.
type stuckDataSource struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stuckDataSource) Observe(ctx context.Context, sv protocol.StreamValues, opts DSOpts) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return errors.New("released")
}

// Test_blobPump_CloseDoesNotHangOnStuckDataSource asserts Close gives up after
// closeTimeout rather than waiting forever on a DataSource that ignores its
// context. The DataSource is shared between the blue and green instances, so a
// Close that never returns would also keep the other instance and the source
// itself from closing.
func Test_blobPump_CloseDoesNotHangOnStuckDataSource(t *testing.T) {
	ds := &stuckDataSource{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(ds.release)

	p := newBlobPump(logger.Test(t), blobPumpParams{
		bbf:                newFakeBroadcaster(),
		ds:                 ds,
		observationTimeout: time.Minute,
		blobLifetimeRounds: DefaultBlobLifetimeRounds,
	})
	require.Equal(t, closeTimeoutSlackMultiplier*time.Minute, p.closeTimeout, "closeTimeout must derive from the observation timeout")
	p.closeTimeout = 100 * time.Millisecond
	p.Start()

	p.SetInput(pumpInputFor(2))
	p.Take(t.Context(), 2)
	select {
	case <-ds.entered:
	case <-time.After(tests.WaitTimeout(t)):
		t.Fatal("DataSource.Observe was never called")
	}

	done := make(chan bool, 1)
	go func() { done <- p.Close() }()
	select {
	case ok := <-done:
		require.False(t, ok, "Close must report the cycle did not unwind")
	case <-time.After(tests.WaitTimeout(t)):
		t.Fatal("Close hung on a DataSource that ignores context")
	}
}

// flakyBroadcaster fails the first failures broadcasts and then delegates. It
// also runs onAttempt before each call, so a test can move the round forward
// between attempts.
type flakyBroadcaster struct {
	*llotest.BlobBroadcastFetcher
	failures  atomic.Int64
	onAttempt func(attempt int)
	attempts  atomic.Int64
}

func (f *flakyBroadcaster) BroadcastBlob(ctx context.Context, payload []byte, hint ocr3_1types.BlobExpirationHint) (ocr3_1types.BlobHandle, error) {
	attempt := int(f.attempts.Add(1))
	if f.onAttempt != nil {
		f.onAttempt(attempt)
	}
	if f.failures.Add(-1) >= 0 {
		return ocr3_1types.BlobHandle{}, errors.New("broadcast unavailable")
	}
	return f.BlobBroadcastFetcher.BroadcastBlob(ctx, payload, hint)
}

// Test_blobPump_RetriesFailedBroadcast covers the recovery this retry exists
// for: values gathered fine are not thrown away because the transport refused
// the first broadcast.
func Test_blobPump_RetriesFailedBroadcast(t *testing.T) {
	ds := mockDS()
	bc := &flakyBroadcaster{BlobBroadcastFetcher: newFakeBroadcaster()}
	bc.failures.Store(1)

	p := testPump(t, ds, bc)
	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(t.Context(), 2)

	require.Eventually(t, func() bool { return p.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.EqualValues(t, 2, bc.attempts.Load(), "the failed attempt must be retried, once")

	snap, reason := p.Take(t.Context(), 3)
	require.NotNil(t, snap, reason)
	require.EqualValues(t, 2, snap.cycleStartSeqNr, "the retry does not change the round the values were gathered for")
}

// Test_blobPump_BroadcastRetryRefreshesExpiry asserts a retry that lands after
// the round moved on hands peers a hint derived from the current round, not
// from the round the values were gathered for.
func Test_blobPump_BroadcastRetryRefreshesExpiry(t *testing.T) {
	ds := mockDS()
	inner := newFakeBroadcaster()
	bc := &flakyBroadcaster{BlobBroadcastFetcher: inner}
	bc.failures.Store(1)

	var p *blobPump
	bc.onAttempt = func(attempt int) {
		if attempt == 1 {
			// Rounds advanced while the first attempt was failing.
			p.SetInput(pumpInputFor(5))
		}
	}
	p = testPump(t, ds, bc)
	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(t.Context(), 2)

	require.Eventually(t, func() bool { return p.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	hints := inner.Hints()
	require.Len(t, hints, 1)
	require.Equal(t, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5 + DefaultBlobLifetimeRounds}, hints[0])

	// Fetchability moved forward. The cycle the values belong to did not.
	snap, reason := p.Take(t.Context(), 3)
	require.NotNil(t, snap, reason)
	require.EqualValues(t, 2, snap.cycleStartSeqNr)
	require.EqualValues(t, 5+DefaultBlobLifetimeRounds, snap.blobExpirationHintSeqNr)
}

// Test_blobPump_BroadcastRetriesAreBounded asserts a transport that stays down
// costs a bounded number of attempts and parks nothing.
func Test_blobPump_BroadcastRetriesAreBounded(t *testing.T) {
	ds := mockDS()
	bc := &flakyBroadcaster{BlobBroadcastFetcher: newFakeBroadcaster()}
	bc.failures.Store(1 << 30)

	p := testPump(t, ds, bc)
	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(t.Context(), 2)

	require.Eventually(t, func() bool { return bc.attempts.Load() >= BlobBroadcastAttempts }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Zero(t, p.Cycles())
	snap, _ := p.Take(t.Context(), 3)
	require.Nil(t, snap)
}

func Test_ResolveBlobLifetimeRounds(t *testing.T) {
	t.Run("applies the default", func(t *testing.T) {
		life, err := ResolveBlobLifetimeRounds(0)
		require.NoError(t, err)
		assert.Equal(t, uint64(DefaultBlobLifetimeRounds), life)
	})

	t.Run("rejects a lifetime below the minimum", func(t *testing.T) {
		_, err := ResolveBlobLifetimeRounds(MinBlobLifetimeRounds - 1)
		require.ErrorContains(t, err, "below MinBlobLifetimeRounds")
	})

	t.Run("rejects a lifetime past the maximum", func(t *testing.T) {
		_, err := ResolveBlobLifetimeRounds(MaxBlobLifetimeRounds + 1)
		require.ErrorContains(t, err, "exceeds MaxBlobLifetimeRounds")
	})

	t.Run("accepts the bounds", func(t *testing.T) {
		for _, v := range []uint64{MinBlobLifetimeRounds, MaxBlobLifetimeRounds} {
			life, err := ResolveBlobLifetimeRounds(v)
			require.NoError(t, err)
			assert.Equal(t, v, life)
		}
	})

	t.Run("the default leaves room for reuse", func(t *testing.T) {
		require.GreaterOrEqual(t, DefaultBlobLifetimeRounds-BlobFetchMarginRounds, SnapshotReuseRounds)
	})
}
