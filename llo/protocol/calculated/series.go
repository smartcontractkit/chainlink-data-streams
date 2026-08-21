package calculated

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// ErrInsufficientHistory is returned when a window holds fewer records than the
// expression asked for. It means "not yet evaluable" — during warmup, after a
// depth increase, or after corrupt state was discarded — and must never be
// treated as zero or silently substituted with a shorter window.
var ErrInsufficientHistory = protocol.ErrInsufficientStreamHistory

// ErrSeriesAsScalar is returned when a window reaches a function that takes
// scalars. Static analysis rejects this at configuration time (history_ast.go),
// so reaching it means the analysis was bypassed; it exists so the failure is
// loud rather than a silently coerced value.
var ErrSeriesAsScalar = errors.New("history window cannot be used as a scalar")

// Series is an immutable window of a stream's past agreed values, oldest first,
// with the timestamp each value was observed at.
//
// Timestamps travel with the values rather than being derived from position:
// rounds are not evenly spaced (stalls, leader changes, minimum report
// intervals), so any time-weighted function or gap check needs the real
// observation times. This is also why _timestamp is not a rangeable field —
// the timestamps are already here.
type Series struct {
	values     []decimal.Decimal
	timestamps []uint64
}

// NewSeries builds a series from parallel value and timestamp slices. It is
// exported for tests and for history readers outside this package.
func NewSeries(values []decimal.Decimal, timestamps []uint64) (Series, error) {
	if len(values) != len(timestamps) {
		return Series{}, fmt.Errorf("series has %d values but %d timestamps", len(values), len(timestamps))
	}
	for i := 1; i < len(timestamps); i++ {
		if timestamps[i] <= timestamps[i-1] {
			return Series{}, fmt.Errorf("series timestamps must be strictly increasing: index %d (%d) is not after index %d (%d)",
				i, timestamps[i], i-1, timestamps[i-1])
		}
	}
	return Series{values: values, timestamps: timestamps}, nil
}

// Len is the number of values in the window.
func (s Series) Len() int { return len(s.values) }

// Values returns the window's values, oldest first. The slice must be treated
// as read-only.
func (s Series) Values() []decimal.Decimal { return s.values }

// Timestamps returns the observation time of each value in nanoseconds,
// parallel to Values and strictly increasing. The slice must be treated as
// read-only.
func (s Series) Timestamps() []uint64 { return s.timestamps }

// Newest returns the most recent value.
func (s Series) Newest() (decimal.Decimal, error) {
	if len(s.values) == 0 {
		return decimal.Decimal{}, fmt.Errorf("%w: window is empty", ErrInsufficientHistory)
	}
	return s.values[len(s.values)-1], nil
}

// Oldest returns the least recent value.
func (s Series) Oldest() (decimal.Decimal, error) {
	if len(s.values) == 0 {
		return decimal.Decimal{}, fmt.Errorf("%w: window is empty", ErrInsufficientHistory)
	}
	return s.values[0], nil
}

func (s Series) String() string {
	return fmt.Sprintf("Series(len=%d)", len(s.values))
}

// Count returns the number of values in a history window.
//
// An empty window is an error rather than a count of zero, matching every other
// window function: a window is only bound once it holds exactly the requested
// depth, so an empty one means something upstream went wrong, and a zero
// flowing into a stream value would hide that.
func Count(x any) (decimal.Decimal, error) {
	series, err := window("Count", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return decimal.NewFromInt(int64(series.Len())), nil
}

// scalarsOrWindow resolves the arguments of a function that accepts either a
// single history window or a list of scalars.
//
// The two forms are deliberately not mixed: Avg(History(s1, 10), s2) would be
// ambiguous about whether the scalar is one more sample or a weight, so it is
// rejected rather than given a meaning.
func scalarsOrWindow(name string, args []any) ([]decimal.Decimal, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%s requires at least one argument", name)
	}

	if series, ok := args[0].(Series); ok {
		if len(args) > 1 {
			return nil, fmt.Errorf("%s takes either a single history window or a list of scalars, not both", name)
		}
		if series.Len() == 0 {
			return nil, fmt.Errorf("%s: history window is empty", name)
		}
		return series.Values(), nil
	}

	values := make([]decimal.Decimal, 0, len(args))
	for _, arg := range args {
		value, err := toDecimal(arg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		values = append(values, value)
	}
	return values, nil
}

// window resolves the single history window argument of a window-only function.
func window(name string, x any) (Series, error) {
	series, ok := x.(Series)
	if !ok {
		return Series{}, fmt.Errorf("%s expects a history window, got %T", name, x)
	}
	if series.Len() == 0 {
		return Series{}, fmt.Errorf("%s: history window is empty", name)
	}
	return series, nil
}

// windowSize resolves a function's sample-count argument against a window.
func windowSize(name string, series Series, x any) (int, error) {
	d, err := toDecimal(x)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	// Compared as a decimal, before any narrowing: silently averaging over fewer
	// samples than asked for would change the meaning of the result, and an
	// oversized value narrowed first would wrap into the accepted range.
	if d.GreaterThan(decimal.NewFromInt(int64(series.Len()))) {
		return 0, fmt.Errorf("%s: sample count %s exceeds the window length %d", name, d, series.Len())
	}
	n, err := decimalToInt("sample count", d, 1, int64(series.Len()))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// HistoryReader is the read side of persisted stream history.
//
// Implementations must be memoized: one underlying state read per
// (streamID, aggregator) pair per round no matter how many channels or
// expressions ask, since that is what keeps history within the per-round state
// budget. Projection to a field and to a depth is cheap and happens per call.
//
// A nil HistoryReader means history is unavailable (the v30 plugin has no
// replicated key-value state), and expressions using History must then fail
// closed rather than evaluate against an empty window.
type HistoryReader interface {
	// Series returns the newest count records of a pair, projected to a field.
	// It must return ErrInsufficientHistory when fewer than count records are
	// stored.
	Series(streamID llotypes.StreamID, aggregator llotypes.Aggregator, count uint32, field Field) (Series, error)
}

// syntheticHistoryReader satisfies every window request with a deterministic
// generated series. It exists so expressions using History can be validated
// offline, where no persisted state exists — the shape of the expression is what
// is being checked, not the values.
//
// It always returns exactly the requested depth, so validation exercises the
// evaluable path rather than the warmup gate.
type syntheticHistoryReader struct {
	// endNanoseconds is the exclusive upper bound of the synthesized
	// timestamps, which must be the round's observation timestamp: functions
	// that place records into a window relative to it (TWAP) would otherwise see
	// every record fall outside the window.
	endNanoseconds uint64
	// intervalNanoseconds is the spacing between synthesized records.
	intervalNanoseconds uint64
}

func (r syntheticHistoryReader) Series(streamID llotypes.StreamID, _ llotypes.Aggregator, count uint32, _ Field) (Series, error) {
	values := make([]decimal.Decimal, 0, count)
	timestamps := make([]uint64, 0, count)

	// The newest record sits one interval before the anchor, so the whole window
	// lands inside a window measured backwards from it.
	oldest := r.endNanoseconds - uint64(count)*r.intervalNanoseconds
	for i := range count {
		// Values vary per record and per stream so that a validated expression
		// cannot accidentally depend on them being equal or zero.
		values = append(values, decimal.New(int64(1_000_000+uint64(streamID)+uint64(i)), -3))
		timestamps = append(timestamps, oldest+uint64(i)*r.intervalNanoseconds)
	}
	return NewSeries(values, timestamps)
}

// SeriesFromRecords projects stored history records onto a single field. One
// stored window serves every field, so this is where History(s1, N),
// History(s1_bid, N) and History(s1_ask, N) diverge.
//
// It is exported so history readers in other packages (the v31 plugin) can
// build a Series without duplicating the field and type handling.
func SeriesFromRecords(records []protocol.StreamHistoryRecord, field Field) (Series, error) {
	values := make([]decimal.Decimal, 0, len(records))
	timestamps := make([]uint64, 0, len(records))
	for i, record := range records {
		value, err := projectStreamValue(record.Value, field)
		if err != nil {
			return Series{}, fmt.Errorf("history record %d: %w", i, err)
		}
		values = append(values, value)
		timestamps = append(timestamps, record.ObservedAtNanoseconds)
	}
	return NewSeries(values, timestamps)
}

// projectStreamValue extracts one field from a stored stream value.
//
// Field selection mirrors how scalar stream values are bound into the
// environment: a quote exposes _bid/_benchmark/_ask and no bare value, so
// asking for the bare value of a quote is an error rather than a silent choice
// of one of the three.
func projectStreamValue(value protocol.StreamValue, field Field) (decimal.Decimal, error) {
	switch v := value.(type) {
	case *protocol.Decimal:
		if field != FieldValue {
			return decimal.Decimal{}, fmt.Errorf("stream value is a decimal and has no %s field", field)
		}
		return v.Decimal(), nil

	case *protocol.Quote:
		switch field {
		case FieldBid:
			return v.Bid, nil
		case FieldAsk:
			return v.Ask, nil
		case FieldBenchmark:
			return v.Benchmark, nil
		default:
			return decimal.Decimal{}, errors.New("stream value is a quote; use the _bid, _ask or _benchmark field")
		}

	case *protocol.TimestampedStreamValue:
		// The timestamp is already carried per record, so only the wrapped
		// value is projected.
		if v.StreamValue == nil {
			return decimal.Decimal{}, protocol.ErrNilStreamValue
		}
		return projectStreamValue(v.StreamValue, field)

	case nil:
		return decimal.Decimal{}, protocol.ErrNilStreamValue

	default:
		return decimal.Decimal{}, fmt.Errorf("unsupported stream value type %T in history", value)
	}
}
