package llo

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"
)

// Stream history metrics.
//
// These are node-local observations with no effect on consensus: nothing reads
// them back, and a node that fails to record one behaves identically. They exist
// because the interesting history conditions are otherwise invisible — a channel
// that is quietly not reporting looks the same as one with nothing to say.
//
// Cardinality is bounded by MaxHistoryPairs (128) times the aggregators in use,
// which is why the per-pair gauges are labelled by stream and aggregator while
// the fault counters are labelled by DON only.
var (
	// historyRecordsMetric is how deep each pair's window actually is. Compared
	// against historyRequiredMetric it shows warmup progress.
	historyRecordsMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "records",
		Help:      "Number of records currently stored in a stream history window.",
	}, []string{"donID", "streamID", "aggregator"})

	// historyRequiredMetric is the depth the live channels ask for.
	historyRequiredMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "required_records",
		Help:      "Depth required of a stream history window by the deepest live channel referencing it.",
	}, []string{"donID", "streamID", "aggregator"})

	// historySatisfiedMetric is 1 once a window is deep enough to be read.
	// Warmup is expected to end; this not reaching 1 is what to alert on.
	historySatisfiedMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "satisfied",
		Help:      "1 when a stream history window holds at least the required number of records, 0 while warming up.",
	}, []string{"donID", "streamID", "aggregator"})

	// historyBytesMetric is what each pair actually wrote this round, which is
	// the quantity the per-round byte budget is spent on. Under the chunked
	// layout that is the newest chunk plus the header, not the whole window, so
	// it should stay flat as a window deepens.
	historyBytesMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "bytes",
		Help:      "Bytes written to the key-value state for a stream history window in the last round.",
	}, []string{"donID", "streamID", "aggregator"})

	// historyChunkWritesMetric counts chunk values written. Expect one per pair
	// per round with an appended value; a sustained higher rate would mean
	// sealed chunks are being rewritten, which the layout forbids.
	historyChunkWritesMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "chunk_writes_total",
		Help:      "Stream history chunks written to the key-value state.",
	}, []string{"donID"})

	// historyPairsMetric is how many pairs hold history, against MaxHistoryPairs.
	historyPairsMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "pairs",
		Help:      "Number of (stream, aggregator) pairs with stored history.",
	}, []string{"donID"})

	// historyDeniedMetric counts pairs refused history because a cap or the byte
	// budget was reached. Any non-zero value means channels are not reporting.
	historyDeniedMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "denied_total",
		Help:      "Pairs denied stream history because the pair cap or byte budget was exhausted. Channels reading them cannot report.",
	}, []string{"donID"})

	// historyCorruptMetric counts windows discarded as undecodable. Stored state
	// is untrusted input, so this is handled rather than fatal, but it should
	// never happen.
	historyCorruptMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "corrupt_total",
		Help:      "Stream history windows discarded because they could not be decoded, and re-warmed from empty.",
	}, []string{"donID"})

	// historyOversizedRecordMetric counts values too large to store. The record
	// is dropped, leaving a gap in the series.
	historyOversizedRecordMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "oversized_records_total",
		Help:      "Agreed values dropped from stream history because they exceeded the per-record size cap, leaving a gap in the series.",
	}, []string{"donID"})

	// historyInsufficientMetric counts rounds a channel could not be evaluated
	// because a window was still too shallow. Expected during warmup; sustained
	// beyond it means the channel is not reporting.
	historyInsufficientMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "llo",
		Subsystem: "history",
		Name:      "insufficient_total",
		Help:      "Rounds in which a channel was not evaluated because a stream history window was shallower than requested.",
	}, []string{"donID"})
)

// captureHistoryTelemetry records the round's history state.
//
// Called after Flush so the sizes reported are the ones actually written. Like
// the rest of the telemetry in this package it is best-effort and side-effect
// free with respect to the protocol.
func (p *Plugin) captureHistoryTelemetry(history *historyStore, requirements historyRequirements) {
	if history == nil {
		return
	}
	donID := strconv.FormatUint(uint64(p.DonID), 10)

	historyPairsMetric.WithLabelValues(donID).Set(float64(len(history.index)))
	historyCorruptMetric.WithLabelValues(donID).Add(float64(len(history.corrupt)))
	historyDeniedMetric.WithLabelValues(donID).Add(float64(len(requirements.denied)))
	historyOversizedRecordMetric.WithLabelValues(donID).Add(float64(history.oversized))
	historyChunkWritesMetric.WithLabelValues(donID).Add(float64(history.chunkWrites))

	for _, key := range history.indexKeys() {
		h, ok := history.windows[key]
		if !ok {
			continue // not touched this round, so nothing new to report
		}
		streamID := strconv.FormatUint(uint64(key.streamID), 10)
		aggregator := strconv.FormatUint(uint64(key.aggregator), 10)

		records, required := h.Len(), h.RequiredCount()
		historyRecordsMetric.WithLabelValues(donID, streamID, aggregator).Set(float64(records))
		historyRequiredMetric.WithLabelValues(donID, streamID, aggregator).Set(float64(required))

		satisfied := 0.0
		if required > 0 && uint32(records) >= required {
			satisfied = 1
		}
		historySatisfiedMetric.WithLabelValues(donID, streamID, aggregator).Set(satisfied)
		historyBytesMetric.WithLabelValues(donID, streamID, aggregator).Set(float64(history.written[key]))
	}

	// Pairs whose history was reclaimed this round are deleted from the per-pair
	// gauges. Prometheus keeps every label set it has ever seen, so leaving them
	// behind does two things: the series report their final value forever, so an
	// alert on an unsatisfied window fires for a pair that no longer exists; and
	// cardinality grows with the number of distinct pairs ever configured, which
	// MaxHistoryPairs does not bound because it caps live pairs only.
	for _, key := range history.reclaimed {
		streamID := strconv.FormatUint(uint64(key.streamID), 10)
		aggregator := strconv.FormatUint(uint64(key.aggregator), 10)
		historyRecordsMetric.DeleteLabelValues(donID, streamID, aggregator)
		historyRequiredMetric.DeleteLabelValues(donID, streamID, aggregator)
		historySatisfiedMetric.DeleteLabelValues(donID, streamID, aggregator)
		historyBytesMetric.DeleteLabelValues(donID, streamID, aggregator)
	}
}

// captureInsufficientHistory counts channels that could not be evaluated this
// round because a window they read was shallower than they asked for.
//
// It is measured per channel and per reference rather than from the per-pair
// gauges, because the stored depth is the maximum any channel wants: a channel
// asking for less can be satisfied while the pair as a whole is not. Counting
// from the gauges would over-report.
func (p *Plugin) captureInsufficientHistory(defs llotypes.ChannelDefinitions, opts *protocol.OptsCache, history *historyStore) {
	if history == nil {
		return
	}

	var unsatisfied int
	for cid, cd := range defs {
		if cd.Tombstone || cd.ReportFormat != llotypes.ReportFormatEVMABIEncodeUnpackedExpr {
			continue
		}
		aggByStream, err := calculated.AggregatorByStream(cd)
		if err != nil {
			continue // reported elsewhere; not a history condition
		}
		expressions, err := calculated.Expressions(opts, cd, cid)
		if err != nil {
			continue
		}
		if !channelHistorySatisfied(expressions, aggByStream, history) {
			unsatisfied++
		}
	}
	if unsatisfied > 0 {
		historyInsufficientMetric.WithLabelValues(strconv.FormatUint(uint64(p.DonID), 10)).Add(float64(unsatisfied))
	}
}

// channelHistorySatisfied reports whether every window a channel's expressions
// read is deep enough to be evaluated.
func channelHistorySatisfied(expressions []string, aggByStream map[llotypes.StreamID]llotypes.Aggregator, history *historyStore) bool {
	for _, expression := range expressions {
		refs, err := calculated.AnalyzeExpressionHistory(expression)
		if err != nil {
			continue // invalid expression, not a history condition
		}
		for _, ref := range refs {
			aggregator, ok := aggByStream[ref.StreamID]
			if !ok {
				continue
			}
			// Reads through the memoized store, so this costs no extra state
			// access.
			h, err := history.Load(ref.StreamID, aggregator)
			if err != nil || uint32(h.Len()) < ref.Count {
				return false
			}
		}
	}
	return true
}
