package llo

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
)

// TestCaptureHistoryTelemetry_DeletesReclaimedPairs pins the per-pair gauge
// lifecycle. Prometheus keeps every label set it has ever seen, so a pair whose
// history is reclaimed has to be deleted from the gauges: otherwise it reports
// its final value forever — an alert on an unsatisfied window would fire for a
// pair that no longer exists — and cardinality grows with the number of distinct
// pairs ever configured, which MaxHistoryPairs does not bound because it caps
// live pairs only.
//
// Deletion is asserted through DeleteLabelValues, which reports whether it
// removed anything: after telemetry has run for the reclaiming round, there must
// be nothing left to remove.
func TestCaptureHistoryTelemetry_DeletesReclaimedPairs(t *testing.T) {
	kv := newCountingKV()
	key := histKey{streamID: 4242, aggregator: testAggMedian}

	p := &Plugin{Logger: logger.Test(t), DonID: 7}
	donID := strconv.FormatUint(uint64(p.DonID), 10)
	streamID := strconv.FormatUint(uint64(key.streamID), 10)
	aggregator := strconv.FormatUint(uint64(key.aggregator), 10)

	// Round one: the pair holds history, so the gauges carry its labels.
	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(key.streamID, key.aggregator, 1))
	_, err := s.Append(key.streamID, key.aggregator, 1_000, testDecimal(1))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))
	p.captureHistoryTelemetry(s, historyRequirements{})

	// Round two: nothing requires the pair, so Flush reclaims it.
	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.Flush(kv))
	require.Contains(t, s.reclaimed, key, "a pair with no capacity must be reclaimed")
	p.captureHistoryTelemetry(s, historyRequirements{})

	assert.False(t, historyRecordsMetric.DeleteLabelValues(donID, streamID, aggregator),
		"records gauge must not outlive the pair")
	assert.False(t, historyRequiredMetric.DeleteLabelValues(donID, streamID, aggregator),
		"required gauge must not outlive the pair")
	assert.False(t, historySatisfiedMetric.DeleteLabelValues(donID, streamID, aggregator),
		"satisfied gauge must not outlive the pair")
	assert.False(t, historyBytesMetric.DeleteLabelValues(donID, streamID, aggregator),
		"bytes gauge must not outlive the pair")
}
