package llo

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	metricObservationChannelFilterDecisionSkip    = "skip"
	metricObservationChannelFilterDecisionInclude = "include"
)

// observationChannelFilterDecisionTotal counts non-tombstone channels per Observation
// classified by ShouldObserveChannelAtObservationStart: included (streams fetched or
// attempted) vs skipped (only time gates block fetching).
var observationChannelFilterDecisionTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "llo",
		Name:      "observation_channel_filter_decisions_total",
		Help:      "Counts non-tombstone channels per Observation: skip = time gates only blocked fetch; include = fetch streams for this channel.",
	},
	[]string{"decision"},
)
