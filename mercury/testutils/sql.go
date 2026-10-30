package testutils

import (
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/sqlutil"
	"github.com/smartcontractkit/chainlink-common/pkg/sqlutil/pg"
	"github.com/smartcontractkit/chainlink-common/pkg/sqlutil/sqltest"
	"github.com/smartcontractkit/chainlink-common/pkg/utils"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
)

func NewSqlxDB(t testing.TB) *sqlx.DB {
	SkipShortDB(t)
	dbURL := os.Getenv("CL_DATABASE_URL")
	if dbURL == "" {
		t.Errorf("you must provide a CL_DATABASE_URL environment variable")
		return nil
	}
	return sqltest.NewDB(t, dbURL)
}

// SkipShortDB skips tb during -short runs, and notes the DB dependency.
func SkipShortDB(tb testing.TB) {
	tests.SkipShort(tb, "DB dependency")
}

func MustExec(t *testing.T, ds sqlutil.DataSource, stmt string, args ...interface{}) {
	require.NoError(t, utils.JustError(ds.ExecContext(t.Context(), stmt, args...)))
}

// pristineDBName is a clean copy of test DB with migrations.
const pristineDBName = "chainlink_test_pristine" // TODO update when splitting schemas

// NewIndependentSqlxDB return a new independent test database, which does not use txdb and therefore supports txs etc.
// Use this with caution, as it is much more costly than NewSqlxDB.
func NewIndependentSqlxDB(t testing.TB) *sqlx.DB {
	return NewIndependentSqlxDBs(t, 1)[0]
}

// NewIndependentSqlxDBs returns n handles to a single fresh test database, which does not use txdb and therefore
// supports real transactions and lock contention between connections. Use this with caution, as it is much more
// costly than NewSqlxDB.
func NewIndependentSqlxDBs(t testing.TB, n int) []*sqlx.DB {
	ctx := tests.Context(t)
	newDBURL := newIndependentDBURL(t)

	dbs := make([]*sqlx.DB, n)
	for i := range dbs {
		db, err := pg.DBConfig{
			IdleInTxSessionTimeout: time.Hour,
			LockTimeout:            15 * time.Second,
			MaxOpenConns:           100,
			MaxIdleConns:           10,
		}.New(ctx, newDBURL.String(), pg.DriverPostgres)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		dbs[i] = db
	}

	return dbs
}

// NewIndependentSqlxSessions returns n handles to a single fresh test database, each pinned to its own
// connection (pool size 1). Unlike the handles from NewIndependentSqlxDBs, session state such as SET or an
// open transaction does not leak between them, which makes it suitable for tests that hold locks on one
// session while another session contends for them.
//
// lock_timeout is set so that a regression that makes a query wait on a lock fails the test instead of hanging it.
func NewIndependentSqlxSessions(t testing.TB, n int) []*sqlx.DB {
	ctx := tests.Context(t)
	newDBURL := newIndependentDBURL(t)

	sessions := make([]*sqlx.DB, n)
	for i := range sessions {
		db, err := pg.DBConfig{
			IdleInTxSessionTimeout: time.Hour,
			LockTimeout:            10 * time.Second,
			MaxOpenConns:           1,
			MaxIdleConns:           1,
		}.New(ctx, newDBURL.String(), pg.DriverPostgres)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		sessions[i] = db
	}

	return sessions
}

func newIndependentDBURL(t testing.TB) url.URL {
	SkipShortDB(t)

	dbURL, err := url.Parse(sqltest.TestURL(t))
	require.NoError(t, err)
	dbName := "chainlink_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	return sqltest.CreateOrReplace(t, *dbURL, dbName, pristineDBName)
}
