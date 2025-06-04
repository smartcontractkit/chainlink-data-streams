package llo

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type mockPredecessorRetirementReportCache struct {
	retirementReports map[ocr2types.ConfigDigest][]byte
	err               error
}

var _ PredecessorRetirementReportCache = &mockPredecessorRetirementReportCache{}

func (p *mockPredecessorRetirementReportCache) AttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest) ([]byte, error) {
	return p.retirementReports[predecessorConfigDigest], p.err
}
func (p *mockPredecessorRetirementReportCache) CheckAttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest, attestedRetirementReport []byte) (RetirementReport, error) {
	panic("not implemented")
}

func Test_Observation(t *testing.T) {
	for _, codec := range []OutcomeCodec{protoOutcomeCodecV0{}, protoOutcomeCodecV1{}} {
		t.Run(fmt.Sprintf("OutcomeCodec: %T", codec), func(t *testing.T) {
			testObservation(t, codec)
		})
	}
}

func testObservation(t *testing.T, outcomeCodec OutcomeCodec) {
	testStartTS := time.Now()
	testStartTSNanos := uint64(testStartTS.UnixNano()) //nolint:gosec // safe cast in tests

	smallDefinitions := map[llotypes.ChannelID]llotypes.ChannelDefinition{
		1: {
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
		},
		2: {
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorMedian}},
		},
	}
	cdc := &mockChannelDefinitionCache{definitions: smallDefinitions}

	ds := &mockDataSource{
		s: map[llotypes.StreamID]StreamValue{
			1: ToDecimal(decimal.NewFromInt(1000)),
			3: ToDecimal(decimal.NewFromInt(3000)),
			4: ToDecimal(decimal.NewFromInt(4000)),
		},
		err: nil,
	}

	obsCodec, err := NewProtoObservationCodec(logger.Nop(), true)
	require.NoError(t, err)

	p := &Plugin{
		Config:                 Config{true},
		OutcomeCodec:           outcomeCodec,
		ShouldRetireCache:      &mockShouldRetireCache{},
		ChannelDefinitionCache: cdc,
		Logger:                 logger.Test(t),
		ObservationCodec:       obsCodec,
		DataSource:             ds,
	}
	var query types.Query // query is always empty for LLO

	t.Run("seqNr=0 always errors", func(t *testing.T) {
		outctx := ocr3types.OutcomeContext{}
		_, err := p.Observation(context.Background(), outctx, query)
		require.EqualError(t, err, "got invalid seqnr=0, must be >=1")
	})

	t.Run("seqNr=1 always returns empty observation", func(t *testing.T) {
		outctx := ocr3types.OutcomeContext{SeqNr: 1}
		obs, err := p.Observation(context.Background(), outctx, query)
		require.NoError(t, err)
		require.Empty(t, obs)
	})

	t.Run("observes timestamp and channel definitions on seqNr=2", func(t *testing.T) {
		outctx := ocr3types.OutcomeContext{SeqNr: 2}
		obs, err := p.Observation(context.Background(), outctx, query)
		require.NoError(t, err)
		decoded, err := p.ObservationCodec.Decode(obs)
		require.NoError(t, err)

		assert.Empty(t, decoded.AttestedPredecessorRetirement)
		assert.False(t, decoded.ShouldRetire)
		assert.Empty(t, decoded.RemoveChannelIDs)
		assert.Empty(t, decoded.StreamValues)
		assert.Equal(t, cdc.definitions, decoded.UpdateChannelDefinitions)
		assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
	})

	t.Run("observes streams on seqNr=2", func(t *testing.T) {
		previousOutcome := Outcome{
			LifeCycleStage:                  llotypes.LifeCycleStage("test"),
			ObservationTimestampNanoseconds: testStartTSNanos,
			ChannelDefinitions:              cdc.definitions,
			ValidAfterNanoseconds:           nil,
			StreamAggregates:                nil,
		}
		encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
		require.NoError(t, err)

		outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
		obs, err := p.Observation(context.Background(), outctx, query)
		require.NoError(t, err)
		decoded, err := p.ObservationCodec.Decode(obs)
		require.NoError(t, err)

		assert.Empty(t, decoded.AttestedPredecessorRetirement)
		assert.False(t, decoded.ShouldRetire)
		assert.Empty(t, decoded.UpdateChannelDefinitions)
		assert.Empty(t, decoded.RemoveChannelIDs)
		assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
		assert.Equal(t, ds.s, decoded.StreamValues)
	})

	mediumDefinitions := map[llotypes.ChannelID]llotypes.ChannelDefinition{
		1: {
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
		},
		3: {
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorMedian}},
		},
		4: {
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorMedian}},
		},
		5: {
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorMedian}},
		},
		6: {
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorMedian}},
		},
	}

	cdc.definitions = mediumDefinitions

	t.Run("votes to increase channel amount by a small amount, and remove one", func(t *testing.T) {
		previousOutcome := Outcome{
			LifeCycleStage:                  llotypes.LifeCycleStage("test"),
			ObservationTimestampNanoseconds: testStartTSNanos,
			ChannelDefinitions:              smallDefinitions,
			ValidAfterNanoseconds:           nil,
			StreamAggregates:                nil,
		}
		encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
		require.NoError(t, err)

		outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
		obs, err := p.Observation(context.Background(), outctx, query)
		require.NoError(t, err)
		decoded, err := p.ObservationCodec.Decode(obs)
		require.NoError(t, err)

		assert.Empty(t, decoded.AttestedPredecessorRetirement)
		assert.False(t, decoded.ShouldRetire)

		assert.Len(t, decoded.UpdateChannelDefinitions, 4)
		assert.ElementsMatch(t, []uint32{3, 4, 5, 6}, maps.Keys(decoded.UpdateChannelDefinitions))
		expected := make(llotypes.ChannelDefinitions)
		for k, v := range mediumDefinitions {
			if k > 2 { // 2 was removed and 1 already present
				expected[k] = v
			}
		}
		assert.Equal(t, expected, decoded.UpdateChannelDefinitions)

		assert.Len(t, decoded.RemoveChannelIDs, 1)
		assert.Equal(t, map[uint32]struct{}{2: {}}, decoded.RemoveChannelIDs)

		assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
		assert.Equal(t, ds.s, decoded.StreamValues)
	})

	largeSize := uint32(100)
	require.Greater(t, int(largeSize), MaxObservationUpdateChannelDefinitionsLength)
	largeDefinitions := make(map[llotypes.ChannelID]llotypes.ChannelDefinition, largeSize)
	for i := uint32(0); i < largeSize; i++ {
		largeDefinitions[i] = llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      []llotypes.Stream{{StreamID: i, Aggregator: llotypes.AggregatorMedian}, {StreamID: (i * 10000), Aggregator: llotypes.AggregatorMedian}, {StreamID: (i * 100000), Aggregator: llotypes.AggregatorMedian}},
		}
	}
	cdc.definitions = largeDefinitions

	t.Run("votes to add channels when channel definitions increases by a large amount, and replace some existing channels with different definitions", func(t *testing.T) {
		t.Run("first round of additions", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              smallDefinitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
			obs, err := p.Observation(context.Background(), outctx, query)
			require.NoError(t, err)
			decoded, err := p.ObservationCodec.Decode(obs)
			require.NoError(t, err)

			assert.Empty(t, decoded.AttestedPredecessorRetirement)
			assert.False(t, decoded.ShouldRetire)

			// Even though we have a large amount of channel definitions, we should
			// only add/replace MaxObservationUpdateChannelDefinitionsLength at a time
			assert.Len(t, decoded.UpdateChannelDefinitions, MaxObservationUpdateChannelDefinitionsLength)
			expected := make(llotypes.ChannelDefinitions)
			for i := uint32(0); i < MaxObservationUpdateChannelDefinitionsLength; i++ {
				expected[i] = largeDefinitions[i]
			}

			// 1 and 2 are actually replaced since definition is different from the one in smallDefinitions
			assert.ElementsMatch(t, []uint32{0, 1, 2, 3, 4}, maps.Keys(decoded.UpdateChannelDefinitions))
			assert.Equal(t, expected, decoded.UpdateChannelDefinitions)

			// Nothing removed
			assert.Empty(t, decoded.RemoveChannelIDs)

			assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
			assert.Equal(t, ds.s, decoded.StreamValues)
		})

		t.Run("second round of additions", func(t *testing.T) {
			offset := uint32(MaxObservationUpdateChannelDefinitionsLength * 2)

			subsetDfns := make(llotypes.ChannelDefinitions)
			for i := uint32(0); i < offset; i++ {
				subsetDfns[i] = largeDefinitions[i]
			}

			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              subsetDfns,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
			obs, err := p.Observation(context.Background(), outctx, query)
			require.NoError(t, err)
			decoded, err := p.ObservationCodec.Decode(obs)
			require.NoError(t, err)

			assert.Empty(t, decoded.AttestedPredecessorRetirement)
			assert.False(t, decoded.ShouldRetire)

			// Even though we have a large amount of channel definitions, we should
			// only add/replace MaxObservationUpdateChannelDefinitionsLength at a time
			assert.Len(t, decoded.UpdateChannelDefinitions, MaxObservationUpdateChannelDefinitionsLength)
			expected := make(llotypes.ChannelDefinitions)
			expectedChannelIDs := []uint32{}
			for i := uint32(0); i < MaxObservationUpdateChannelDefinitionsLength; i++ {
				expectedChannelIDs = append(expectedChannelIDs, i+offset)
				expected[i+offset] = largeDefinitions[i+offset]
			}
			assert.Equal(t, expected, decoded.UpdateChannelDefinitions)

			assert.ElementsMatch(t, expectedChannelIDs, maps.Keys(decoded.UpdateChannelDefinitions))

			// Nothing removed
			assert.Empty(t, decoded.RemoveChannelIDs)

			assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
			assert.Equal(t, ds.s, decoded.StreamValues)
		})

		t.Run("in case previous outcome channel definitions is invalid, returns error", func(t *testing.T) {
			dfns := make(llotypes.ChannelDefinitions)
			for i := uint32(0); i < 2*MaxOutcomeChannelDefinitionsLength; i++ {
				dfns[i] = llotypes.ChannelDefinition{
					ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
					Streams:      []llotypes.Stream{{StreamID: i, Aggregator: llotypes.AggregatorMedian}, {StreamID: (i * 10000), Aggregator: llotypes.AggregatorMedian}, {StreamID: (i * 100000), Aggregator: llotypes.AggregatorMedian}},
				}
			}

			previousOutcome := Outcome{
				ChannelDefinitions: dfns,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
			_, err = p.Observation(context.Background(), outctx, query)
			require.EqualError(t, err, "previousOutcome.Definitions is invalid: too many channels, got: 4000/2000")
		})

		t.Run("in case ChannelDefinitionsCache returns invalid definitions, does not vote to change anything", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              smallDefinitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			dfns := make(llotypes.ChannelDefinitions)
			for i := uint32(0); i < 2*MaxOutcomeChannelDefinitionsLength; i++ {
				dfns[i] = llotypes.ChannelDefinition{
					ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
					Streams:      []llotypes.Stream{{StreamID: i, Aggregator: llotypes.AggregatorMedian}, {StreamID: (i * 10000), Aggregator: llotypes.AggregatorMedian}, {StreamID: (i * 100000), Aggregator: llotypes.AggregatorMedian}},
				}
			}
			cdc.definitions = dfns

			outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
			obs, err := p.Observation(context.Background(), outctx, query)
			require.NoError(t, err)
			decoded, err := p.ObservationCodec.Decode(obs)
			require.NoError(t, err)

			assert.Empty(t, decoded.UpdateChannelDefinitions)
			assert.Empty(t, decoded.RemoveChannelIDs)
		})
	})

	cdc.definitions = smallDefinitions

	t.Run("votes to remove channel IDs", func(t *testing.T) {
		t.Run("first round of removals", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              largeDefinitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
			obs, err := p.Observation(context.Background(), outctx, query)
			require.NoError(t, err)
			decoded, err := p.ObservationCodec.Decode(obs)
			require.NoError(t, err)

			assert.Empty(t, decoded.AttestedPredecessorRetirement)
			assert.False(t, decoded.ShouldRetire)
			// will have two items here to account for the change of 1 and 2 in smallDefinitions
			assert.Len(t, decoded.UpdateChannelDefinitions, 2)

			// Even though we have a large amount of channel definitions, we should
			// only remove MaxObservationRemoveChannelIDsLength at a time
			assert.Len(t, decoded.RemoveChannelIDs, MaxObservationRemoveChannelIDsLength)
			assert.ElementsMatch(t, []uint32{0, 3, 4, 5, 6}, maps.Keys(decoded.RemoveChannelIDs))

			assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
			assert.Equal(t, ds.s, decoded.StreamValues)
		})
		t.Run("second round of removals", func(t *testing.T) {
			offset := uint32(MaxObservationUpdateChannelDefinitionsLength * 2)

			subsetDfns := maps.Clone(largeDefinitions)
			for i := uint32(0); i < offset; i++ {
				delete(subsetDfns, i)
			}

			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              subsetDfns,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
			obs, err := p.Observation(context.Background(), outctx, query)
			require.NoError(t, err)
			decoded, err := p.ObservationCodec.Decode(obs)
			require.NoError(t, err)

			assert.Empty(t, decoded.AttestedPredecessorRetirement)
			assert.False(t, decoded.ShouldRetire)
			// will have two items here to account for the change of 1 and 2 in smallDefinitions
			assert.Len(t, decoded.UpdateChannelDefinitions, 2)

			// Even though we have a large amount of channel definitions, we should
			// only remove MaxObservationRemoveChannelIDsLength at a time
			assert.Len(t, decoded.RemoveChannelIDs, MaxObservationRemoveChannelIDsLength)
			assert.ElementsMatch(t, []uint32{10, 11, 12, 13, 14}, maps.Keys(decoded.RemoveChannelIDs))

			assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
			assert.Equal(t, ds.s, decoded.StreamValues)
		})
	})

	t.Run("sets shouldRetire if ShouldRetireCache.ShouldRetire() is true", func(t *testing.T) {
		previousOutcome := Outcome{}
		src := &mockShouldRetireCache{shouldRetire: true}
		p.ShouldRetireCache = src
		encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
		require.NoError(t, err)

		outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: encodedPreviousOutcome}
		obs, err := p.Observation(context.Background(), outctx, query)
		require.NoError(t, err)
		decoded, err := p.ObservationCodec.Decode(obs)
		require.NoError(t, err)

		assert.True(t, decoded.ShouldRetire)
	})

	t.Run("when predecessor config digest is set", func(t *testing.T) {
		cd := types.ConfigDigest{2, 3, 4, 5, 6}
		p.PredecessorConfigDigest = &cd
		t.Run("in staging lifecycle stage, adds attestedRetirementReport to observation", func(t *testing.T) {
			prrc := &mockPredecessorRetirementReportCache{
				retirementReports: map[ocr2types.ConfigDigest][]byte{
					{2, 3, 4, 5, 6}: []byte("foo"),
				},
			}
			p.PredecessorRetirementReportCache = prrc
			previousOutcome := Outcome{
				LifeCycleStage:                  LifeCycleStageStaging,
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              cdc.definitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
			obs, err := p.Observation(context.Background(), outctx, query)
			require.NoError(t, err)
			decoded, err := p.ObservationCodec.Decode(obs)
			require.NoError(t, err)

			assert.Equal(t, []byte("foo"), decoded.AttestedPredecessorRetirement)
		})
		t.Run("if predecessor retirement report cache returns error, returns error", func(t *testing.T) {
			prrc := &mockPredecessorRetirementReportCache{
				err: errors.New("retirement report not found error"),
			}
			p.PredecessorRetirementReportCache = prrc
			previousOutcome := Outcome{
				LifeCycleStage:                  LifeCycleStageStaging,
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              cdc.definitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
			_, err = p.Observation(context.Background(), outctx, query)
			require.EqualError(t, err, "error fetching attested retirement report from cache: retirement report not found error")
		})
		t.Run("in production lifecycle stage, does not add attestedRetirementReport to observation", func(t *testing.T) {
			prrc := &mockPredecessorRetirementReportCache{
				retirementReports: map[ocr2types.ConfigDigest][]byte{
					{2, 3, 4, 5, 6}: []byte("foo"),
				},
				err: nil,
			}
			p.PredecessorRetirementReportCache = prrc
			previousOutcome := Outcome{
				LifeCycleStage:                  LifeCycleStageProduction,
				ObservationTimestampNanoseconds: testStartTSNanos,
				ChannelDefinitions:              cdc.definitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
			obs, err := p.Observation(context.Background(), outctx, query)
			require.NoError(t, err)
			decoded, err := p.ObservationCodec.Decode(obs)
			require.NoError(t, err)

			assert.Equal(t, []byte(nil), decoded.AttestedPredecessorRetirement)
		})
	})
	t.Run("if previous outcome is retired, returns observation with only timestamp", func(t *testing.T) {
		previousOutcome := Outcome{
			LifeCycleStage: LifeCycleStageRetired,
		}
		encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
		require.NoError(t, err)

		outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
		obs, err := p.Observation(context.Background(), outctx, query)
		require.NoError(t, err)
		decoded, err := p.ObservationCodec.Decode(obs)
		require.NoError(t, err)

		assert.Zero(t, decoded.AttestedPredecessorRetirement)
		assert.False(t, decoded.ShouldRetire)
		assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
		assert.Empty(t, decoded.UpdateChannelDefinitions)
		assert.Empty(t, decoded.RemoveChannelIDs)
		assert.Empty(t, decoded.StreamValues)
	})

	invalidDefinitions := map[llotypes.ChannelID]llotypes.ChannelDefinition{
		1: {
			ReportFormat: llotypes.ReportFormatJSON,
			// no streams means invalid
			Streams: []llotypes.Stream{},
		},
	}
	t.Run("if channel definitions file is invalid, does not vote to add or remove any channels and only submits observations", func(t *testing.T) {
		previousOutcome := Outcome{
			LifeCycleStage:     LifeCycleStageStaging,
			ChannelDefinitions: smallDefinitions,
		}
		encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
		require.NoError(t, err)

		cdc.definitions = invalidDefinitions

		outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
		obs, err := p.Observation(context.Background(), outctx, query)
		require.NoError(t, err)
		decoded, err := p.ObservationCodec.Decode(obs)
		require.NoError(t, err)

		assert.Empty(t, decoded.UpdateChannelDefinitions)
		assert.Empty(t, decoded.RemoveChannelIDs)
		assert.GreaterOrEqual(t, decoded.UnixTimestampNanoseconds, testStartTSNanos)
		assert.Equal(t, ds.s, decoded.StreamValues)
	})
}
