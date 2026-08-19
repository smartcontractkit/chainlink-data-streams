package llo

import (
	"fmt"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// optsEchoCodec encodes nothing but the opts it resolved, so a report reveals
// exactly which generation of channel opts the codec was given.
type optsEchoCodec struct{}

type echoOpts struct {
	V int `json:"v"`
}

func (optsEchoCodec) Encode(r protocol.Report, _ llotypes.ChannelDefinition, optsCache *protocol.OptsCache) ([]byte, error) {
	o, err := protocol.GetOpts[echoOpts](optsCache, r.ChannelID)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("v=%d", o.V)), nil
}

func (optsEchoCodec) Verify(llotypes.ChannelDefinition) error { return nil }

var _ protocol.ReportCodec = optsEchoCodec{}

// Test_Reports_StateTransition_Concurrent_OptsIsolation runs Reports for seqNr N
// concurrently with StateTransition for N+1 across a channel-definitions change,
// which is exactly what OCR3.1 does: the two callbacks run in separate
// goroutines (outcome generation vs report attestation) and are free to overlap.
//
// The definitions record the concurrent StateTransition loads is a different one
// from the record the precursor being reported was built from, so any node-wide
// opts state shared between the two would let the encoding depend on goroutine
// timing - nondeterminism in a consensus-critical path.
func Test_Reports_StateTransition_Concurrent_OptsIsolation(t *testing.T) {
	ctx := tests.Context(t)

	withOpts := func(v int) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
			Opts:         llotypes.ChannelOpts(fmt.Sprintf(`{"v":%d}`, v)),
		}
	}
	// A round that agrees on defs (when non-nil) and always observes stream 100,
	// so the channel is reportable once it is in effect.
	obsRound := func(ts uint64, defs llotypes.ChannelDefinitions) []ocrtypes.AttributedObservation {
		obs := Observation{
			UnixTimestampNanoseconds: ts,
			UpdateChannelDefinitions: defs,
			StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, obs)))
		}
		return aos
	}

	// setup replays rounds 1..4 and returns the plugin, the store and the
	// precursor of round 4. Round 4's effective definitions are the record
	// written by round 2 (opts v=1); round 4 itself writes a new record with
	// opts v=2, which round 5's StateTransition then loads.
	setup := func(t *testing.T) (*Plugin, *memKV, ocr3_1types.ReportsPlusPrecursor) {
		p := testPlugin(t)
		p.ReportCodecs = map[llotypes.ReportFormat]protocol.ReportCodec{llotypes.ReportFormatJSON: optsEchoCodec{}}
		kv := newMemKV()

		_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, obsRound(1_000, nil), kv, nil)
		require.NoError(t, err)
		_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, obsRound(2_000, llotypes.ChannelDefinitions{1: withOpts(1)}), kv, nil)
		require.NoError(t, err)
		_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, obsRound(3_000, nil), kv, nil)
		require.NoError(t, err)
		prec4, err := p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, obsRound(4_000, llotypes.ChannelDefinitions{1: withOpts(2)}), kv, nil)
		require.NoError(t, err)

		decoded, err := decodePrecursor(prec4)
		require.NoError(t, err)
		require.Equal(t, uint64(2), decoded.ChannelStateSeqNr, "round 4 must still be reporting the record written by round 2")

		return p, kv, prec4
	}

	// Serial baseline: what the reports must always be.
	p, kv, prec4 := setup(t)
	baseline, err := p.Reports(ctx, 4, prec4)
	require.NoError(t, err)
	require.Len(t, baseline, 1)
	require.Equal(t, "v=1", string(baseline[0].ReportWithInfo.Report))
	_, err = p.StateTransition(ctx, 5, ocrtypes.AttributedQuery{}, obsRound(5_000, nil), kv, nil)
	require.NoError(t, err)

	for i := 0; i < 50; i++ {
		p, kv, prec4 := setup(t)

		var (
			wg     sync.WaitGroup
			got    string
			errRep error
			errST  error
		)

		wg.Add(2)
		go func() {
			defer wg.Done()
			rs, err := p.Reports(ctx, 4, prec4)
			if err != nil {
				errRep = err
				return
			}
			if len(rs) != 1 {
				errRep = fmt.Errorf("expected 1 report, got %d", len(rs))
				return
			}
			got = string(rs[0].ReportWithInfo.Report)
		}()
		go func() {
			defer wg.Done()
			// Round 5 loads the record round 4 wrote (opts v=2).
			_, errST = p.StateTransition(ctx, 5, ocrtypes.AttributedQuery{}, obsRound(5_000, nil), kv, nil)
		}()
		wg.Wait()

		require.NoError(t, errRep)
		require.NoError(t, errST)
		require.Equal(t, "v=1", got, "Reports must encode with the opts of the record its precursor was built from, whatever a concurrent StateTransition loads")
	}
}
