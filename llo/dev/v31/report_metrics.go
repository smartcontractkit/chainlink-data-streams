package llo

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// reportsNotEmittedMetric counts reportable channels that produced no report,
// and backfill rows passed over as unemittable. The state transition advances
// the watermark on reportability alone, so each count is an interval no report
// covers. Node-local, with no effect on consensus.
var reportsNotEmittedMetric = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "llo",
	Subsystem: "reports",
	Name:      "not_emitted_total",
	Help:      "Reportable channels that emitted no report, and backfill rows skipped as unemittable. Each is an interval no report covers.",
}, []string{"donID", "reportFormat", "reason"})

// reportNotEmitted logs and counts a reportable channel that emitted no report
// this round. Its watermark advances regardless, so the round's interval is lost.
func (p *Plugin) reportNotEmitted(seqNr uint64, channelID llotypes.ChannelID, format llotypes.ReportFormat, reason string, kv ...any) {
	kv = append([]any{"reason", reason, "channelID", channelID, "reportFormat", format, "stage", "Report", "seqNr", seqNr}, kv...)
	p.Logger.Errorw("Reportable channel emitted no report; its interval is not covered", kv...)
	reportsNotEmittedMetric.WithLabelValues(strconv.FormatUint(uint64(p.DonID), 10), format.String(), reason).Inc()
}

// channelsUnreportableMetric counts, per reason, the channels a state transition
// found unreportable for a reason the unreportableTally records. Channels that
// are retired, tombstoned, new, held back by the time gates, or a backfill with
// nothing left to emit are their expected state and not counted. Node-local,
// with no effect on consensus.
var channelsUnreportableMetric = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "llo",
	Subsystem: "channels",
	Name:      "unreportable_total",
	Help:      "Channels a state transition found unreportable, by reason. Counted once per channel per round.",
}, []string{"donID", "reason"})

// count adds the tally to channelsUnreportableMetric.
func (t *unreportableTally) count(donID uint32) {
	if t == nil || len(t.reasons) == 0 {
		return
	}
	don := strconv.FormatUint(uint64(donID), 10)
	for _, r := range t.reasons {
		channelsUnreportableMetric.WithLabelValues(don, r.label).Add(float64(r.count))
	}
}
