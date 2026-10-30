package dataengine

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-data-streams/mercury/testutils"
)

var (
	sURL  = "wss://example.com/mercury"
	sURL2 = "wss://mercuryserver.test"
	sURL3 = "wss://mercuryserver.example/foo"
)

func TestORM(t *testing.T) {
	ctx := t.Context()
	db := testutils.NewSqlxDB(t)

	t.Run("Insert, Get, Delete, Prune", func(t *testing.T) {
		donID := uint32(654321)
		orm := NewORM(db, donID)

		t.Run("DonID", func(t *testing.T) {
			assert.Equal(t, donID, orm.DonID())
		})
		const n = 10
		transmissions := makeSampleTransmissions(n, sURL)
		// Insert
		err := orm.Insert(ctx, transmissions)
		require.NoError(t, err)
		// Get limits
		result, err := orm.Get(ctx, sURL, 0, 0)
		require.NoError(t, err)
		assert.Empty(t, result)

		// Get limits
		result, err = orm.Get(ctx, sURL, 1, 0)
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, transmissions[len(transmissions)-1], result[0])

		result, err = orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)

		assert.ElementsMatch(t, transmissions, result)

		result, err = orm.Get(ctx, "other server url", 100, 0)
		require.NoError(t, err)
		assert.Empty(t, result)
		// Delete
		err = orm.Delete(ctx, [][32]byte{transmissions[0].Hash()})
		require.NoError(t, err)

		result, err = orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)

		require.Len(t, result, n-1)
		assert.NotContains(t, result, transmissions[0])
		assert.Contains(t, result, transmissions[1])

		err = orm.Delete(ctx, [][32]byte{transmissions[1].Hash()})
		require.NoError(t, err)

		result, err = orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)
		require.Len(t, result, n-2)
		// Prune
		// ensure that len(transmissions) exceeds batch size to test batching
		err = orm.Insert(ctx, transmissions)
		require.NoError(t, err)

		d, err := orm.Prune(ctx, sURL, 1, n/3)
		require.NoError(t, err)
		assert.Equal(t, int64(n-1), d)

		result, err = orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, transmissions[len(transmissions)-1], result[0])

		// Prune again, should not delete anything
		d, err = orm.Prune(ctx, sURL, 1, n/3)
		require.NoError(t, err)
		assert.Zero(t, d)
		result, err = orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, transmissions[len(transmissions)-1], result[0])

		// Pruning with max allowed records = 0 deletes everything
		d, err = orm.Prune(ctx, sURL, 0, 1)
		require.NoError(t, err)
		assert.Equal(t, int64(1), d)
		result, err = orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)
		require.Empty(t, result)
	})

	t.Run("Prune trims to exactly the correct number of records", func(t *testing.T) {
		donID := uint32(100)
		orm := NewORM(db, donID)

		var transmissions []*Transmission
		// create 100 records (10 * 10 duplicate sequence numbers)
		for seqNr := range uint64(10) {
			for i := range 10 {
				transmissions = append(transmissions, makeSampleTransmission(seqNr, sURL, []byte{byte(i)}))
			}
		}
		err := orm.Insert(ctx, transmissions)
		require.NoError(t, err)

		d, err := orm.Prune(ctx, sURL, 43, 3)
		require.NoError(t, err)
		assert.Equal(t, int64(57), d)

		result, err := orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)
		require.Len(t, result, 43)
		assert.Equal(t, uint64(9), result[0].SeqNr)
	})

	t.Run("Get respects maxAge argument and does not retrieve records older than this", func(t *testing.T) {
		donID := uint32(101)
		orm := NewORM(db, donID)

		transmissions := makeSampleTransmissions(10, sURL)
		err := orm.Insert(ctx, transmissions)
		require.NoError(t, err)

		testutils.MustExec(t, db, `UPDATE llo_mercury_transmit_queue SET inserted_at = NOW() - INTERVAL '1 year' WHERE seq_nr < 5`)

		// Get with maxAge = 0 should return all records
		result, err := orm.Get(ctx, sURL, 100, 0)
		require.NoError(t, err)
		require.Len(t, result, 10)

		// Get with maxAge = 1 month should return only the records with seq_nr >= 5
		result, err = orm.Get(ctx, sURL, 100, 30*24*time.Hour)
		require.NoError(t, err)
		require.Len(t, result, 5)
	})
}

// TestORM_PruneSkipsRowsLockedByDelete checks that Prune does not wait for rows that a concurrent Delete has
// locked. Delete removes rows by transmission_hash while Prune walks them by seq_nr, so without
// FOR UPDATE SKIP LOCKED the two delete paths can hold locks on rows the other wants and deadlock (40P01).
func TestORM_PruneSkipsRowsLockedByDelete(t *testing.T) {
	ctx := t.Context()
	sessions := testutils.NewIndependentSqlxSessions(t, 2)
	lockDS, pruneDS := sessions[0], sessions[1]

	const (
		donID     = uint32(1234)
		n         = 10
		maxSize   = 5
		batchSize = 100
	)
	transmissions := makeSampleTransmissions(n, sURL)
	require.NoError(t, NewORM(lockDS, donID).Insert(ctx, transmissions))

	// Hold a lock on the oldest row, exactly like an in-flight Delete would.
	tx, err := lockDS.BeginTxx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, tx.GetContext(ctx, new([]byte),
		`SELECT transmission_hash FROM llo_mercury_transmit_queue
		WHERE don_id = $1 AND server_url = $2 AND seq_nr = 0
		FOR UPDATE`, donID, sURL))

	// Prune must not block on the locked row. It deletes everything it can and leaves that row to the Delete
	// that is already removing it. If it blocks, the session's lock_timeout fires and it returns 55P03, so the
	// failure is reported as an error rather than a hang; the deadline is only a safety net.
	done := make(chan struct{})
	var (
		deleted  int64
		pruneErr error
	)
	go func() {
		deleted, pruneErr = NewORM(pruneDS, donID).Prune(ctx, sURL, maxSize, batchSize)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Prune did not return; expected FOR UPDATE SKIP LOCKED to skip the locked row")
	}
	require.NoError(t, pruneErr, "Prune blocked on a row locked by Delete; expected FOR UPDATE SKIP LOCKED")
	assert.Equal(t, int64(n-maxSize-1), deleted)

	result, err := NewORM(pruneDS, donID).Get(ctx, sURL, 100, 0)
	require.NoError(t, err)
	require.Len(t, result, n-maxSize+1, "maxSize rows plus the one still locked")
	var seqNrs []uint64
	for _, tr := range result {
		seqNrs = append(seqNrs, tr.SeqNr)
	}
	assert.ElementsMatch(t, []uint64{0, 5, 6, 7, 8, 9}, seqNrs, "only the locked row should be left below maxSize")
}

// TestORM_ConcurrentDeleteAndPrune hammers Delete and Prune at the same time and fails if either returns an
// error. It is a broader regression net around the SQL in Prune; the deadlock itself is reproduced
// deterministically in TestORM_PruneSkipsRowsLockedByDelete.
func TestORM_ConcurrentDeleteAndPrune(t *testing.T) {
	ctx := t.Context()
	sessions := testutils.NewIndependentSqlxSessions(t, 2)
	deleteORM := NewORM(sessions[0], 4321)
	pruneORM := NewORM(sessions[1], 4321)

	const (
		n          = 1_000
		iterations = 25
		maxSize    = n / 2
		pruneBatch = 32
		deleteSize = 100
	)

	transmissions := makeSampleTransmissions(n, sURL)
	require.NoError(t, deleteORM.Insert(ctx, transmissions))
	hashes := make([][32]byte, n)
	for i, tr := range transmissions {
		hashes[i] = tr.Hash()
	}

	errs := make(chan error, 2*iterations)
	for i := range iterations {
		// Restore anything the previous round pruned so both paths keep contending over real rows.
		require.NoError(t, deleteORM.Insert(ctx, transmissions))

		// Rotate the Delete batch so it overlaps different parts of Prune's seq_nr ordering.
		start := (i * 37) % n
		batch := make([][32]byte, deleteSize)
		for j := range batch {
			batch[j] = hashes[(start+j)%n]
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := deleteORM.Delete(ctx, batch); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := pruneORM.Prune(ctx, sURL, maxSize, pruneBatch); err != nil {
				errs <- err
			}
		}()
		wg.Wait()
	}
	close(errs)
	for err := range errs {
		require.NoError(t, err, "concurrent Delete/Prune must not deadlock")
	}
}
