package transmitter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// Test_onTransmit_NotifyRecoversListenerPanic asserts a panicking listener is
// contained: listeners run on their own goroutines, so an unrecovered panic
// would crash the plugin, and other listeners would never be notified.
func Test_onTransmit_NotifyRecoversListenerPanic(t *testing.T) {
	lggr, observedLogs := logger.TestObserved(t, zapcore.ErrorLevel)
	o := &onTransmit{lggr: lggr}
	done := make(chan struct{})

	o.OnTransmit(func(digest types.ConfigDigest, seqNr uint64) { panic("listener boom") })
	o.OnTransmit(func(digest types.ConfigDigest, seqNr uint64) {
		require.Equal(t, uint64(42), seqNr)
		close(done)
	})

	o.notify(types.ConfigDigest{1}, 42)

	select {
	case <-done:
	case <-tests.Context(t).Done():
		t.Fatal("healthy listener was not notified")
	}

	// Wait for the recovery log so the panicking goroutine cannot outlive the
	// test and log against a finished *testing.T.
	require.Eventually(t, func() bool {
		return observedLogs.FilterMessage("Transmit listener panicked").Len() == 1
	}, tests.WaitTimeout(t), 10*time.Millisecond)
	assert.Equal(t, "listener boom", observedLogs.FilterMessage("Transmit listener panicked").All()[0].ContextMap()["panic"])
}
