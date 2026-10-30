package llo

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// precursorSizeMetric exports the size of the precursor this node produced in
// its latest state transition, before and after framing. Their ratio is the
// compression the precursor gets in practice. Node-local, with no effect on
// consensus.
var precursorSizeMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "llo",
	Subsystem: "precursor",
	Name:      "size_bytes",
	Help:      "Size of the latest precursor: raw is the marshaled proto, framed is what libocr digests, stores and serves to peers.",
}, []string{"donID", "encoding"})

// precursorMetrics holds the label children for one DON, resolved once at
// plugin construction.
type precursorMetrics struct {
	raw, framed prometheus.Gauge
}

func newPrecursorMetrics(donID uint32) *precursorMetrics {
	don := strconv.FormatUint(uint64(donID), 10)
	return &precursorMetrics{
		raw:    precursorSizeMetric.WithLabelValues(don, "raw"),
		framed: precursorSizeMetric.WithLabelValues(don, "framed"),
	}
}

// record exports one precursor's sizes. A nil receiver records nothing, for
// plugins built without the factory.
func (m *precursorMetrics) record(raw, framed int) {
	if m == nil {
		return
	}
	m.raw.Set(float64(raw))
	m.framed.Set(float64(framed))
}
