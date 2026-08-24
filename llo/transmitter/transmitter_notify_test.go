package transmitter

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// Test_onTransmit_NotifyRecoversListenerPanic asserts a panicking listener is
// contained: listeners run on their own goroutines, so an unrecovered panic
// would crash the plugin, and other listeners would never be notified.
func Test_onTransmit_NotifyRecoversListenerPanic(t *testing.T) {
	o := &onTransmit{lggr: logger.Test(t)}
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
}
