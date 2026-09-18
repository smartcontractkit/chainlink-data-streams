package llo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31/llotest"
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

func testPump(t *testing.T, ds DataSource, bbf ocr3_1types.BlobBroadcastFetcher, maxAge time.Duration) *blobPump {
	t.Helper()
	p := newBlobPump(logger.Test(t), blobPumpParams{
		bbf:                bbf,
		ds:                 ds,
		configDigest:       ocrtypes.ConfigDigest{1},
		verboseLogging:     true,
		observationTimeout: tests.WaitTimeout(t),
		maxSnapshotAge:     maxAge,
		maxSnapshotRounds:  DefaultMaxSnapshotRounds,
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
	p := testPump(t, ds, bc, time.Minute)

	p.SetInput(pumpInputFor(2))
	snap, reason := p.Take(2)
	require.Nil(t, snap)
	require.NotEmpty(t, reason)

	require.Eventually(t, func() bool { return p.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)

	p.SetInput(pumpInputFor(3))
	snap, reason = p.Take(3)
	require.NotNil(t, snap, "reason: %s", reason)
	require.Equal(t, uint64(2), snap.forSeqNr)
	require.Equal(t, uint64(2+DefaultMaxSnapshotRounds), snap.usableBefore)
	require.Equal(t, uint64(2+DefaultBlobLifetimeRounds), snap.expiresAt)
	require.NotEmpty(t, snap.handleBytes)
	require.Equal(t, uint64(1), p.Misses())

	// The blob was hinted to expire at forSeqNr + lifetime. Assert on the first
	// broadcast: later cycles re-broadcast an identical payload, which is
	// content-addressed to the same handle, so only the ordered hint log
	// distinguishes them.
	hints := bc.Hints()
	require.NotEmpty(t, hints)
	require.Equal(t, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: snap.expiresAt}, hints[0])

	// The snapshot's handle fetches back the payload the pump broadcast.
	var handle ocr3_1types.BlobHandle
	require.NoError(t, handle.UnmarshalBinary(snap.handleBytes))
	payload, err := bc.FetchBlob(tests.Context(t), handle)
	require.NoError(t, err)
	require.NotEmpty(t, payload)

	// Taking a snapshot also kicks, so the pump keeps running.
	require.Eventually(t, func() bool { return p.Cycles() >= 2 }, tests.WaitTimeout(t), 10*time.Millisecond)
}

// Test_blobPump_TakeIsSingleUse guards against two rounds referencing the same
// blob handle.
func Test_blobPump_TakeIsSingleUse(t *testing.T) {
	p := testPump(t, mockDS(), newFakeBroadcaster(), time.Minute)
	p.SetInput(pumpInputFor(2))
	require.Eventually(t, func() bool {
		_, _ = p.Take(2)
		return p.Cycles() >= 1
	}, tests.WaitTimeout(t), 10*time.Millisecond)

	require.Eventually(t, func() bool {
		snap, _ := p.Take(2)
		return snap != nil
	}, tests.WaitTimeout(t), 10*time.Millisecond)

	// The pump may have parked a fresh snapshot by now, so assert on the parked
	// slot directly rather than on a second Take.
	p.mu.Lock()
	p.ready = nil
	p.mu.Unlock()
	snap, reason := p.Take(2)
	require.Nil(t, snap)
	require.NotEmpty(t, reason)
}

func Test_blobPump_RejectsStaleSnapshots(t *testing.T) {
	// The local freshness gate is usableBefore, which falls well short of the
	// blob's own expiry: the snapshot stops being referenceable while the blob
	// is still fetchable by peers.
	t.Run("too stale by sequence number", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster(), time.Minute)
		p.mu.Lock()
		p.ready = &blobSnapshot{handleBytes: []byte{1}, observedAt: time.Now(), forSeqNr: 2, usableBefore: 4, expiresAt: 6}
		p.mu.Unlock()

		snap, reason := p.Take(4)
		require.Nil(t, snap)
		require.Contains(t, reason, "too stale")
		require.Equal(t, uint64(1), p.Misses())
	})

	t.Run("expired by wall clock", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster(), time.Nanosecond)
		p.mu.Lock()
		p.ready = &blobSnapshot{handleBytes: []byte{1}, observedAt: time.Now().Add(-time.Hour), forSeqNr: 2, usableBefore: 100, expiresAt: 100}
		p.mu.Unlock()

		snap, reason := p.Take(3)
		require.Nil(t, snap)
		require.Contains(t, reason, "too old")
	})

	t.Run("age check disabled", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster(), -1)
		p.mu.Lock()
		p.ready = &blobSnapshot{handleBytes: []byte{1}, observedAt: time.Now().Add(-time.Hour), forSeqNr: 2, usableBefore: 100, expiresAt: 100}
		p.mu.Unlock()

		snap, _ := p.Take(3)
		require.NotNil(t, snap, "with the age check disabled only maxSnapshotRounds bounds staleness")
	})

	// With no explicit age the bound is derived from the measured round period,
	// so a pump that has seen no rounds yet must not reject on age: guessing a
	// cadence would silently stop the node contributing stream values.
	t.Run("derived age check is inert until a round period is measured", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster(), 0)
		p.mu.Lock()
		p.ready = &blobSnapshot{handleBytes: []byte{1}, observedAt: time.Now().Add(-time.Hour), forSeqNr: 2, usableBefore: 100, expiresAt: 100}
		p.mu.Unlock()

		snap, reason := p.Take(3)
		require.NotNil(t, snap, "reason: %s", reason)
	})

	t.Run("derived age check rejects once the round period is known", func(t *testing.T) {
		p := testPump(t, mockDS(), newFakeBroadcaster(), 0)
		p.mu.Lock()
		p.roundPeriod = time.Millisecond
		p.ready = &blobSnapshot{handleBytes: []byte{1}, observedAt: time.Now().Add(-time.Hour), forSeqNr: 2, usableBefore: 100, expiresAt: 100}
		p.mu.Unlock()

		snap, reason := p.Take(3)
		require.Nil(t, snap)
		require.Contains(t, reason, "too old")
	})
}

// Test_blobPump_ParksNothingOnFailure covers both failure modes: a data-source
// error and a broadcast error. Neither may park a snapshot, since stream values
// are only ever disseminated by blob.
func Test_blobPump_ParksNothingOnFailure(t *testing.T) {
	t.Run("data source error", func(t *testing.T) {
		ds := mockDS()
		ds.err = errors.New("bridge down")
		p := testPump(t, ds, newFakeBroadcaster(), time.Minute)
		p.SetInput(pumpInputFor(2))
		_, _ = p.Take(2)

		require.Eventually(t, func() bool { return ds.observeCount() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
		require.Zero(t, p.Cycles())
		snap, _ := p.Take(3)
		require.Nil(t, snap)
	})

	t.Run("broadcast error", func(t *testing.T) {
		bc := func() *llotest.BlobBroadcastFetcher {
			bc := newFakeBroadcaster()
			bc.SetBroadcastError(errors.New("broadcast unavailable"))
			return bc
		}()
		p := testPump(t, mockDS(), bc, time.Minute)
		p.SetInput(pumpInputFor(2))
		_, _ = p.Take(2)

		require.Eventually(t, func() bool { return bc.Broadcasts() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
		require.Zero(t, p.Cycles())
		snap, _ := p.Take(3)
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
			p := testPump(t, ds, bc, time.Minute)
			p.SetInput(in)
			for i := 0; i < 3; i++ {
				_, _ = p.Take(2)
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
	p := testPump(t, ds, newFakeBroadcaster(), time.Minute)
	p.SetInput(pumpInputFor(2))

	for i := 0; i < 10; i++ {
		_, _ = p.Take(2)
	}
	require.Eventually(t, func() bool { return ds.started() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Never(t, func() bool { return ds.concurrent() > 1 }, 100*time.Millisecond, 10*time.Millisecond)
	close(release)
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
		p := testPump(t, ds, nil, time.Minute)
		p.SetInput(pumpInputFor(2))
		for i := 0; i < 3; i++ {
			snap, reason := p.Take(2)
			require.Nil(t, snap)
			require.Equal(t, "blob pump disabled", reason)
		}
		require.Never(t, func() bool { return ds.observeCount() > 0 }, 100*time.Millisecond, 10*time.Millisecond)
		require.Zero(t, p.Cycles())
	})

	t.Run("nil data source", func(t *testing.T) {
		bc := newFakeBroadcaster()
		p := testPump(t, nil, bc, time.Minute)
		p.SetInput(pumpInputFor(2))
		snap, reason := p.Take(2)
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
	p := testPump(t, ds, newFakeBroadcaster(), time.Minute)

	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(2)
	require.Eventually(t, func() bool { return ds.calls.Load() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Zero(t, p.Cycles())

	// Pump goroutine is still alive: a second kick still reaches the DataSource.
	p.SetInput(pumpInputFor(3))
	_, _ = p.Take(3)
	require.Eventually(t, func() bool { return ds.calls.Load() >= 2 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.False(t, p.inFlight.Load())
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
		maxSnapshotRounds:  DefaultMaxSnapshotRounds,
		blobLifetimeRounds: DefaultBlobLifetimeRounds,
	})
	require.Equal(t, closeTimeoutSlackMultiplier*time.Minute, p.closeTimeout, "closeTimeout must derive from the observation timeout")
	p.closeTimeout = 100 * time.Millisecond
	p.Start()

	p.SetInput(pumpInputFor(2))
	p.Take(2)
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

	p := testPump(t, ds, bc, time.Minute)
	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(2)

	require.Eventually(t, func() bool { return p.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.EqualValues(t, 2, bc.attempts.Load(), "the failed attempt must be retried, once")

	snap, reason := p.Take(3)
	require.NotNil(t, snap, reason)
	require.EqualValues(t, 2, snap.forSeqNr, "the retry does not change the round the values were gathered for")
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
	p = testPump(t, ds, bc, time.Minute)
	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(2)

	require.Eventually(t, func() bool { return p.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	hints := inner.Hints()
	require.Len(t, hints, 1)
	require.Equal(t, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5 + DefaultBlobLifetimeRounds}, hints[0])

	// Fetchability moved forward; local freshness did not.
	snap, reason := p.Take(3)
	require.NotNil(t, snap, reason)
	require.EqualValues(t, 2+DefaultMaxSnapshotRounds, snap.usableBefore)
	require.EqualValues(t, 5+DefaultBlobLifetimeRounds, snap.expiresAt)
}

// Test_blobPump_BroadcastRetriesAreBounded asserts a transport that stays down
// costs a bounded number of attempts and parks nothing.
func Test_blobPump_BroadcastRetriesAreBounded(t *testing.T) {
	ds := mockDS()
	bc := &flakyBroadcaster{BlobBroadcastFetcher: newFakeBroadcaster()}
	bc.failures.Store(1 << 30)

	p := testPump(t, ds, bc, time.Minute)
	p.SetInput(pumpInputFor(2))
	_, _ = p.Take(2)

	require.Eventually(t, func() bool { return bc.attempts.Load() >= BlobBroadcastAttempts }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Zero(t, p.Cycles())
	snap, _ := p.Take(3)
	require.Nil(t, snap)
}
