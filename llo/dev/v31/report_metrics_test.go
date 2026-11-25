package llo

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

func Test_unreportableTally_count(t *testing.T) {
	tally := &unreportableTally{}
	tally.note("nil_stream_value", "nil stream value", 1)
	tally.note("nil_stream_value", "nil stream value", 2)
	tally.note("codec_support_insufficient", "too few oracles advertise a report codec for this format", 3)

	tally.count(4343)
	tally.count(4343)

	assert.InDelta(t, 4, counterValue(t, channelsUnreportableMetric.WithLabelValues("4343", "nil_stream_value")), 0, "every channel counts, each round")
	assert.InDelta(t, 2, counterValue(t, channelsUnreportableMetric.WithLabelValues("4343", "codec_support_insufficient")), 0)
}

func Test_unreportableTally_countNil(t *testing.T) {
	var tally *unreportableTally
	tally.count(4344)
	(&unreportableTally{}).count(4344)
	assert.InDelta(t, 0, counterValue(t, channelsUnreportableMetric.WithLabelValues("4344", "nil_stream_value")), 0)
}
