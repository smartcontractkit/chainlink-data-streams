package calculated

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

func TestValidateExpression(t *testing.T) {
	t.Parallel()

	t.Run("accepts", func(t *testing.T) {
		t.Parallel()
		for _, expression := range []string{
			"Add(s1, s2)",
			"Count(History(s1, 10))",
			"Avg(History(s1_bid, 300))",
			"Div(Avg(History(s1, 10)), s2)",
			"EMA(History(s1, 50), 20)",
			`TWAP(History(s1, 600), {window: Duration("5m"), minSamples: 240, maxHeadGap: 30, maxInteriorGap: 10, maxTailGap: 30})`,
		} {
			assert.NoError(t, ValidateExpression(expression), "expression %q", expression)
		}
	})

	t.Run("rejects", func(t *testing.T) {
		t.Parallel()
		for _, expression := range []string{
			"",
			"Add(History(s1, 10), 1)",
			"Count(History(s1, 0))",
			"Count(History(s1_timestamp, 10))",
			"Count(History(s1, x))",
			"Avg(s1__h10)",
			"Count(History(s1, 10)",
			fmt.Sprintf("Count(History(s1, %d))", protocol.MaxHistoryRecordsPerPair+1),
		} {
			assert.Error(t, ValidateExpression(expression), "expression %q", expression)
		}
	})
}

// TestValidateExpression_TWAPSatisfiability covers the static half of TWAP
// validation: a configuration that can never be satisfied is a deployment
// mistake, and saying so here beats letting every round reject the window and
// look like a data problem.
func TestValidateExpression_TWAPSatisfiability(t *testing.T) {
	t.Parallel()

	// 600 records can supply 240 observations.
	require.NoError(t, ValidateExpression(
		`TWAP(History(s1, 600), {window: Duration("5m"), minSamples: 240, maxHeadGap: 30, maxInteriorGap: 10, maxTailGap: 30})`))

	// 100 records can never supply 240.
	err := ValidateExpression(
		`TWAP(History(s1, 100), {window: Duration("5m"), minSamples: 240, maxHeadGap: 30, maxInteriorGap: 10, maxTailGap: 30})`)
	require.ErrorIs(t, err, ErrHistoryExpression)
	assert.Contains(t, err.Error(), "only keeps 100 records")

	// Exactly enough is fine.
	require.NoError(t, ValidateExpression(
		`TWAP(History(s1, 240), {window: Duration("4m"), minSamples: 240, maxHeadGap: 30, maxInteriorGap: 10, maxTailGap: 30})`))

	// A non-literal configuration cannot be checked statically; runtime
	// validation still applies.
	require.NoError(t, ValidateExpression("TWAP(History(s1, 10), cfg)"))

	// Wrong arity is caught.
	require.Error(t, ValidateExpression("TWAP(History(s1, 10))"))
}

func TestValidateChannelExpressions(t *testing.T) {
	t.Parallel()

	channel := func(streams []llotypes.Stream, expressions ...string) llotypes.ChannelDefinition {
		abi := ""
		for i, expression := range expressions {
			if i > 0 {
				abi += ","
			}
			abi += fmt.Sprintf(`{"type":"int256","expression":%q,"expressionStreamID":%d}`, expression, 900+i)
		}
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams:      streams,
			Opts:         []byte(fmt.Sprintf(`{"abi":[%s]}`, abi)),
		}
	}
	median := func(id llotypes.StreamID) llotypes.Stream {
		return llotypes.Stream{StreamID: id, Aggregator: llotypes.AggregatorMedian}
	}

	require.NoError(t, ValidateChannelExpressions(nil,
		channel([]llotypes.Stream{median(1)}, "Count(History(s1, 10))"), 1))

	// Every problem is reported in one pass, not one at a time.
	err := ValidateChannelExpressions(nil,
		channel([]llotypes.Stream{median(1)}, "Add(History(s1, 10), 1)", "Count(History(s1, 0))"), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be passed directly")
	assert.Contains(t, err.Error(), "at least 1")

	// A channel aggregating one stream two ways cannot be validated, because
	// which aggregation a History call means is undecidable.
	err = ValidateChannelExpressions(nil, channel([]llotypes.Stream{
		median(1),
		{StreamID: 1, Aggregator: llotypes.AggregatorMode},
	}, "Count(History(s1, 10))"), 1)
	require.ErrorContains(t, err, "aggregators")

	// Undecodable opts.
	cd := channel([]llotypes.Stream{median(1)}, "Count(History(s1, 10))")
	cd.Opts = []byte(`{"abi":`)
	require.Error(t, ValidateChannelExpressions(nil, cd, 1))

	// An ABI entry with no expression names a calculated stream nothing can
	// produce, so the channel could never report. Rejected at validation rather
	// than left to fail every round.
	cd = llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Streams:      []llotypes.Stream{median(1)},
		Opts:         []byte(`{"abi":[{"type":"int192"}]}`),
	}
	require.ErrorContains(t, ValidateChannelExpressions(nil, cd, 1), "expression is empty")

	// Including when only one of several entries is missing one.
	cd = llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Streams:      []llotypes.Stream{median(1)},
		Opts:         []byte(`{"abi":[{"type":"int192","expression":"Add(s1, 1)","expressionStreamID":900},{"type":"int192"}]}`),
	}
	err = ValidateChannelExpressions(nil, cd, 1)
	require.ErrorContains(t, err, "expression is empty")
	assert.Contains(t, err.Error(), "abi index: 1")
}

// TestProcessCalculatedStreamsDryRun_Satisfiability checks the offline path
// rejects the same configurations the static analysis does.
func TestProcessCalculatedStreamsDryRun_Satisfiability(t *testing.T) {
	t.Parallel()

	require.NoError(t, ProcessCalculatedStreamsDryRun(
		`TWAP(History(s1, 300), {window: Duration("5m"), minSamples: 240, maxHeadGap: 30, maxInteriorGap: 10, maxTailGap: 30})`))

	err := ProcessCalculatedStreamsDryRun(
		`TWAP(History(s1, 10), {window: Duration("5m"), minSamples: 240, maxHeadGap: 30, maxInteriorGap: 10, maxTailGap: 30})`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only keeps 10 records")
}
