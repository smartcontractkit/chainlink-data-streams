package llo

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Stream value freshness.
//
// Every stream value reaches the state transition through a blob gathered by
// the sender's blob pump some rounds earlier. The pump cannot judge on its own
// whether those values are still fresh: a decision taken on one node, from that
// node's clock, is invisible to its peers and withholds the node's values with
// no trace in consensus. The state transition judges instead, from replicated
// data only, so every oracle keeps the same contributions.
//
// A contribution carries the seqNr its cycle started in (CycleStartSeqNr). Two
// ages are derived from it, both from consensus data:
//
//   - round age: seqNr - cycleStartSeqNr, bounded by MaxStreamValueAgeRounds.
//     This is the bound the pump's local round gate used to enforce, now
//     visible to every oracle.
//   - time age: roundTs - agreedTs[cycleStartSeqNr], bounded by
//     MaxStreamValueAgeRounds round intervals. The agreed timestamp of the
//     cycle's round is a lower bound on when the values were gathered, since
//     the cycle started after the previous round committed. It catches a stall:
//     seqNr stands still while time passes, so round age alone would call
//     values gathered before the stall one round old. It is skipped while the
//     round interval is unknown, during the first rounds after a fresh state.
//
// A per stream fallback keeps liveness: when the fresh contributions cannot
// reach minContributions, the ones stale by rounds are aggregated too, as
// before this rule existed. Contributions stale by time never come back: after
// a DON wide stall every contribution is old, and re-admitting them would
// report pre-stall values under the post-stall timestamp. The stream has no
// aggregate that round instead, as it had when the pump withheld the snapshot.
// There is no persisted freshness state and no mode.
//
// The agreed timestamps come from a window the hot state carries, one entry
// per recent round. The same window yields a replicated estimate of the round
// interval, so the time bound tracks the DON's real cadence. The configured
// DeltaRound is only a floor: libocr documents it as a lower bound on the
// round interval, and it may be zero.
const (
	// SnapshotReuseRounds is how many consecutive rounds may reference one
	// blob pump snapshot: gathered during round N, referenced at N+1 through
	// N+SnapshotReuseRounds. The pump does not reuse snapshots yet; the
	// constant sizes the freshness bounds for when it does.
	SnapshotReuseRounds = 4
	// MaxStreamValueAgeRounds bounds, in rounds and in round intervals, how old
	// a contribution's values may be at the round that aggregates them. A
	// snapshot referenced on its last reuse round is SnapshotReuseRounds old
	// before DataSource.Observe time and the window's one interval of
	// imprecision are added, so two intervals of slack are left for those.
	MaxStreamValueAgeRounds = SnapshotReuseRounds + 2
	// agreedTimestampWindow is how many recent agreed timestamps the hot state
	// keeps. It must cover every cycleStartSeqNr a fresh contribution may carry.
	agreedTimestampWindow = MaxStreamValueAgeRounds + 1
)

// streamContribution is one oracle's value for one stream, kept apart from the
// other oracles' values until freshness is decided.
type streamContribution struct {
	value           protocol.StreamValue
	cycleStartSeqNr uint64
}

// freshnessStats is what one round's selection did, for logs and telemetry.
type freshnessStats struct {
	// streamsFiltered is how many streams aggregated fresh contributions only,
	// leaving at least one stale contribution out.
	streamsFiltered int
	// streamsFallenBack is how many streams re-admitted contributions stale by
	// rounds because the fresh ones missed minContributions.
	streamsFallenBack int
	// contributionsStale is how many contributions were judged stale, whether
	// or not the fallback let them in.
	contributionsStale int
	// staleByRounds, staleByTime and staleBeforeWindow split contributionsStale
	// by the bound that rejected first.
	staleByRounds     int
	staleByTime       int
	staleBeforeWindow int
	maxStreamValueAge time.Duration
	roundInterval     time.Duration
}

func (s freshnessStats) any() bool {
	return s.streamsFiltered > 0 || s.streamsFallenBack > 0 || s.contributionsStale > 0
}

// appendAgreedTimestamp returns the window for a round whose agreed timestamp
// is ts, given the previous round's window: ts appended, oldest entries
// dropped beyond agreedTimestampWindow. The result never aliases prev.
func appendAgreedTimestamp(prev []uint64, ts uint64) []uint64 {
	n := min(len(prev)+1, agreedTimestampWindow)
	window := make([]uint64, 0, n)
	window = append(window, prev[max(0, len(prev)+1-n):]...)
	return append(window, ts)
}

// agreedTimestampAt returns the agreed timestamp of round at from a window
// whose last entry belongs to round seqNr. ok is false when at is outside the
// window.
func agreedTimestampAt(window []uint64, seqNr, at uint64) (uint64, bool) {
	if at > seqNr || seqNr-at >= uint64(len(window)) {
		return 0, false
	}
	return window[len(window)-1-int(seqNr-at)], true
}

// roundIntervalEstimate is the median of the nonzero intervals between the
// consecutive agreed timestamps of the window, floored at the configured
// DeltaRound. The median absorbs the one wide gap a stall leaves. Zero when
// fewer than two timestamps are known or every interval is zero, which happens
// while the agreed timestamp is held against a regressing median: the cadence
// is unknown and no time bound applies.
func roundIntervalEstimate(window []uint64, configured time.Duration) time.Duration {
	intervals := make([]uint64, 0, len(window))
	for i := 1; i < len(window); i++ {
		if window[i] > window[i-1] {
			intervals = append(intervals, window[i]-window[i-1])
		}
	}
	if len(intervals) == 0 {
		return 0
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i] < intervals[j] })
	median := time.Duration(min(intervals[len(intervals)/2], uint64(math.MaxInt64))) //nolint:gosec // G115 clamped
	return max(median, configured)
}

// selectFreshContributions decides, per stream, which contributions this round
// aggregates. A contribution is fresh when its round age and its time age are
// both within bound. A roundInterval of zero disables the time bound. See the
// package comment above. The fresh set is used when
// it reaches minContributions, otherwise the contributions stale by rounds are
// re-admitted. Values keep their input order, so the result does not depend on
// how the split went.
//
// Every input is replicated: seqNr, the agreed timestamps, the configured
// interval from the shared OCR config, and a cycle start seqNr the sender can
// only choose within the window. The result is therefore the same on every
// oracle.
func selectFreshContributions(
	byStream map[llotypes.StreamID][]streamContribution,
	seqNr uint64,
	roundTs uint64,
	window []uint64,
	roundInterval time.Duration,
	minContributions int,
) (map[llotypes.StreamID][]protocol.StreamValue, freshnessStats) {
	maxAge := uint64(MaxStreamValueAgeRounds * roundInterval)                                       //nolint:gosec // G115 positive duration
	stats := freshnessStats{maxStreamValueAge: time.Duration(maxAge), roundInterval: roundInterval} //nolint:gosec // G115 bounded above
	out := make(map[llotypes.StreamID][]protocol.StreamValue, len(byStream))

	for sid, contributions := range byStream {
		fresh := make([]protocol.StreamValue, 0, len(contributions))
		// fallback is every contribution the fallback may re-admit: the fresh
		// ones and those stale by rounds only. Stale by time never comes back.
		fallback := make([]protocol.StreamValue, 0, len(contributions))
		stale := 0
		for _, c := range contributions {
			switch {
			case c.cycleStartSeqNr > seqNr || seqNr-c.cycleStartSeqNr > MaxStreamValueAgeRounds:
				stats.staleByRounds++
				fallback = append(fallback, c.value)
			default:
				cycleTs, ok := agreedTimestampAt(window, seqNr, c.cycleStartSeqNr)
				if !ok {
					stats.staleBeforeWindow++
					fallback = append(fallback, c.value)
				} else if maxAge > 0 && roundTs > cycleTs && roundTs-cycleTs > maxAge {
					stats.staleByTime++
				} else {
					fresh = append(fresh, c.value)
					fallback = append(fallback, c.value)
					continue
				}
			}
			stale++
		}
		stats.contributionsStale += stale
		switch {
		case stale == 0:
			out[sid] = fresh
		case len(fresh) >= minContributions || len(fallback) == len(fresh):
			stats.streamsFiltered++
			out[sid] = fresh
		default:
			stats.streamsFallenBack++
			out[sid] = fallback
		}
	}
	return out, stats
}

// Freshness metrics. Node-local observations with no effect on consensus. The
// fallback counter is the signal that the bounds are too tight for a DON:
// streams falling back every round aggregate stale values with no other trace.
var (
	freshnessStaleContributionsMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "llo",
		Subsystem: "freshness",
		Name:      "stale_contributions_total",
		Help:      "Stream value contributions judged stale by the state transition.",
	}, []string{"donID", "reason"})
	freshnessFallbackStreamsMetric = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "llo",
		Subsystem: "freshness",
		Name:      "fallback_streams_total",
		Help:      "Streams that re-admitted contributions stale by rounds because the fresh ones could not reach min contributions.",
	}, []string{"donID"})
	freshnessRoundIntervalMetric = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "llo",
		Subsystem: "freshness",
		Name:      "round_interval_seconds",
		Help:      "Round interval estimated from the agreed timestamps of recent rounds, as used for the stream value age bound.",
	}, []string{"donID"})
)

func (s freshnessStats) record(donID uint32) {
	don := strconv.FormatUint(uint64(donID), 10)
	if s.staleByRounds > 0 {
		freshnessStaleContributionsMetric.WithLabelValues(don, "rounds").Add(float64(s.staleByRounds))
	}
	if s.staleByTime > 0 {
		freshnessStaleContributionsMetric.WithLabelValues(don, "time").Add(float64(s.staleByTime))
	}
	if s.staleBeforeWindow > 0 {
		freshnessStaleContributionsMetric.WithLabelValues(don, "window").Add(float64(s.staleBeforeWindow))
	}
	if s.streamsFallenBack > 0 {
		freshnessFallbackStreamsMetric.WithLabelValues(don).Add(float64(s.streamsFallenBack))
	}
	freshnessRoundIntervalMetric.WithLabelValues(don).Set(s.roundInterval.Seconds())
}
