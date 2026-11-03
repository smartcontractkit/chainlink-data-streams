package llo

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, g.Write(&m))
	return m.GetGauge().GetValue()
}

func Test_Plugin_encodePrecursor_RecordsSizes(t *testing.T) {
	p := &Plugin{precursorMetrics: newPrecursorMetrics(4242)}
	raw, err := marshalPrecursor(goldenPrecursor())
	require.NoError(t, err)

	framed, err := p.encodePrecursor(goldenPrecursor())
	require.NoError(t, err)

	assert.InDelta(t, float64(len(raw)), gaugeValue(t, precursorSizeMetric.WithLabelValues("4242", "raw")), 0)
	assert.InDelta(t, float64(len(framed)), gaugeValue(t, precursorSizeMetric.WithLabelValues("4242", "framed")), 0)
}

func Test_Plugin_encodePrecursor_NilMetrics(t *testing.T) {
	_, err := (&Plugin{}).encodePrecursor(goldenPrecursor())
	require.NoError(t, err)
}
