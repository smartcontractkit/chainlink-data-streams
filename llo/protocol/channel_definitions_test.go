package protocol

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type mockReportCodec struct {
	err error
}

func (m mockReportCodec) Encode(Report, llotypes.ChannelDefinition, *OptsCache) ([]byte, error) {
	return nil, nil
}

func (m mockReportCodec) Verify(llotypes.ChannelDefinition) error {
	return m.err
}

func Test_VerifyChannelDefinitions(t *testing.T) {
	mockReportFormat := llotypes.ReportFormat(0)
	codecs := make(map[llotypes.ReportFormat]ReportCodec)
	codecs[mockReportFormat] = mockReportCodec{}
	codecs[llotypes.ReportFormatHistoryBackfill] = ReportCodecHistoryBackfill{}

	t.Run("fails with too many channels", func(t *testing.T) {
		channelDefs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength+1)
		for i := uint32(0); i < MaxOutcomeChannelDefinitionsLength+1; i++ {
			channelDefs[i] = llotypes.ChannelDefinition{}
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "too many channels, got: 2001/2000")
	})
	t.Run("fails if channel has too many streams", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: make([]llotypes.Stream, MaxStreamsPerChannel+1),
			},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 has too many streams, got: 10001/10000")
	})
	t.Run("fails for channel with no streams", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 has no streams")
	})

	t.Run("fails for channel with zero aggregator", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{llotypes.Stream{}},
			},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 has stream 0 with zero aggregator (this may indicate an uninitialized struct)")
	})

	t.Run("rejects unknown aggregator at admission but not once committed", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{{StreamID: 7, Aggregator: llotypes.Aggregator(99)}},
			},
		}
		err := verifyAdmittingAll(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 has stream 7 with unknown aggregator 99")

		// Already committed: verification must pass, otherwise every oracle
		// fails every round and the protocol halts with no recovery path.
		require.NoError(t, VerifyChannelDefinitions(codecs, channelDefs))
	})

	t.Run("accepts known and calculated aggregators at admission", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{
					{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
					{StreamID: 2, Aggregator: llotypes.AggregatorMode},
					{StreamID: 3, Aggregator: llotypes.AggregatorQuote},
					{StreamID: 4, Aggregator: llotypes.AggregatorCalculated},
				},
			},
		}
		require.NoError(t, verifyAdmittingAll(codecs, channelDefs))
	})

	t.Run("fails if too many total unique stream IDs", func(t *testing.T) {
		streams := make([]llotypes.Stream, MaxObservationStreamValuesLength)
		for i := uint32(0); i < MaxObservationStreamValuesLength; i++ {
			streams[i] = llotypes.Stream{StreamID: i, Aggregator: llotypes.AggregatorMedian}
		}
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: streams,
			},
			2: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{llotypes.Stream{StreamID: MaxObservationStreamValuesLength + 1, Aggregator: llotypes.AggregatorMedian}},
			},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "too many unique stream IDs, got: 10001/10000")
	})
	t.Run("fails if codec.Verify fails", func(t *testing.T) {
		failingCodecs := make(map[llotypes.ReportFormat]ReportCodec)
		failingCodecs[mockReportFormat] = mockReportCodec{err: errors.New("codec error")}
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				ReportFormat: mockReportFormat,
				Streams: []llotypes.Stream{
					llotypes.Stream{
						StreamID:   1,
						Aggregator: llotypes.AggregatorMedian,
					},
				},
			},
		}
		err := VerifyChannelDefinitions(failingCodecs, channelDefs)
		require.EqualError(t, err, "invalid ChannelDefinition with ID 1: codec error")
	})
	t.Run("succeeds with valid channel definitions", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{
					llotypes.Stream{
						StreamID:   1,
						Aggregator: llotypes.AggregatorMedian,
					},
				},
			},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.NoError(t, err)
	})

	t.Run("history_backfill fails when target missing", func(t *testing.T) {
		hfCodecs := map[llotypes.ReportFormat]ReportCodec{
			llotypes.ReportFormatHistoryBackfill: ReportCodecHistoryBackfill{},
			llotypes.ReportFormatJSON:            stubReportCodec{},
		}
		defs := llotypes.ChannelDefinitions{
			10: {
				ReportFormat: llotypes.ReportFormatHistoryBackfill,
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
				Opts:         []byte(`{"targetChannelId":99,"observations":{"1000":{"1":"1"}}}`),
			},
		}
		err := VerifyChannelDefinitions(hfCodecs, defs)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid history backfill channel 10")
	})

	t.Run("history_backfill succeeds", func(t *testing.T) {
		hfCodecs := map[llotypes.ReportFormat]ReportCodec{
			llotypes.ReportFormatHistoryBackfill: ReportCodecHistoryBackfill{},
			llotypes.ReportFormatJSON:            stubReportCodec{},
		}
		streams := []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}
		defs := llotypes.ChannelDefinitions{
			2: {
				ReportFormat: llotypes.ReportFormatJSON,
				Streams:      streams,
			},
			10: {
				ReportFormat: llotypes.ReportFormatHistoryBackfill,
				Streams:      streams,
				Opts:         []byte(`{"targetChannelId":2,"observations":{"1000":{"1":"1"}}}`),
			},
		}
		err := VerifyChannelDefinitions(hfCodecs, defs)
		require.NoError(t, err)
	})

	t.Run("succeeds with exact maxes", func(t *testing.T) {
		streams := make([]llotypes.Stream, MaxObservationStreamValuesLength)
		for i := uint32(0); i < MaxObservationStreamValuesLength; i++ {
			streams[i] = llotypes.Stream{StreamID: i, Aggregator: llotypes.AggregatorMedian}
		}
		channelDefs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength)
		for i := uint32(0); i < MaxOutcomeChannelDefinitionsLength; i++ {
			channelDefs[i] = llotypes.ChannelDefinition{Streams: streams}
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.NoError(t, err)
	})
}

// stubReportCodec is a minimal ReportCodec that accepts any channel definition.
type stubReportCodec struct{}

func (stubReportCodec) Encode(Report, llotypes.ChannelDefinition, *OptsCache) ([]byte, error) {
	return nil, nil
}
func (stubReportCodec) Verify(llotypes.ChannelDefinition) error { return nil }

// twapCodec charges every definition MaxTWAPCallsPerExpression TWAP calls.
type twapCodec struct{ stubReportCodec }

func (twapCodec) TWAPCalls(llotypes.ChannelDefinition) int { return MaxTWAPCallsPerExpression }

// verifyAdmittingAll verifies channelDefs with every channel treated as being
// admitted, which is what the admission-only checks are exercised against.
func verifyAdmittingAll(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions) error {
	admitting := make(map[llotypes.ChannelID]struct{}, len(channelDefs))
	for id := range channelDefs {
		admitting[id] = struct{}{}
	}
	return VerifyChannelDefinitionsForAdmission(codecs, channelDefs, admitting)
}

func Test_VerifyChannelDefinitions_CalculatedStreamCollisions(t *testing.T) {
	codecs := make(map[llotypes.ReportFormat]ReportCodec)

	exprChannel := func(observed llotypes.StreamID, opts string, calculated ...llotypes.StreamID) llotypes.ChannelDefinition {
		streams := []llotypes.Stream{{StreamID: observed, Aggregator: llotypes.AggregatorMedian}}
		for _, sid := range calculated {
			streams = append(streams, llotypes.Stream{StreamID: sid, Aggregator: llotypes.AggregatorCalculated})
		}
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams:      streams,
			Opts:         llotypes.ChannelOpts(opts),
		}
	}

	t.Run("accepts distinct calculated stream IDs", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: exprChannel(10, `{"abi":[{"expressionStreamID":100}]}`, 100),
			2: exprChannel(11, `{"abi":[{"expressionStreamID":101}]}`, 101),
		}
		require.NoError(t, verifyAdmittingAll(codecs, channelDefs))
	})
	t.Run("fails when two channels declare the same calculated stream", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: exprChannel(10, `{"abi":[{"expressionStreamID":100}]}`, 100),
			2: exprChannel(11, `{"abi":[{"expressionStreamID":100}]}`, 100),
		}
		err := verifyAdmittingAll(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 2 declares calculated stream 100 already declared by channel 1")
	})
	t.Run("fails when a channel declares the same calculated stream twice", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: exprChannel(10, `{"abi":[{"expressionStreamID":100},{"expressionStreamID":100}]}`, 100),
		}
		err := verifyAdmittingAll(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 declares calculated stream 100 already declared by channel 1")
	})
	t.Run("fails when a calculated stream collides with an observed stream", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: exprChannel(10, `{"abi":[{"expressionStreamID":100}]}`, 100),
			2: {
				Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
			},
		}
		err := verifyAdmittingAll(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 declares calculated stream 100, which channel 2 observes")
	})
	t.Run("fails when a calculated stream collides with its own channel's observed stream", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: exprChannel(100, `{"abi":[{"expressionStreamID":100}]}`),
		}
		err := verifyAdmittingAll(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 declares calculated stream 100, which channel 1 observes")
	})
	t.Run("fails for undecodable, empty, and zero-ID expression opts", func(t *testing.T) {
		for opts, want := range map[string]string{
			`not json`:                           "invalid ChannelDefinition with ID 1: failed to decode calculated stream opts: json: invalid character o as null",
			`{}`:                                 "invalid ChannelDefinition with ID 1: no expressions found in channel definition",
			`{"abi":[{"expressionStreamID":0}]}`: "invalid ChannelDefinition with ID 1: expression stream ID is 0, abi index: 0",
		} {
			channelDefs := llotypes.ChannelDefinitions{1: exprChannel(10, opts)}
			require.EqualError(t, verifyAdmittingAll(codecs, channelDefs), want)
		}
	})
	t.Run("keeps the streams declared before a zero-ID expression", func(t *testing.T) {
		// Channel 1 is committed and still evaluates stream 100, so channel 2
		// cannot be admitted declaring it.
		channelDefs := llotypes.ChannelDefinitions{
			1: exprChannel(10, `{"abi":[{"expressionStreamID":100},{"expressionStreamID":0}]}`, 100),
			2: exprChannel(11, `{"abi":[{"expressionStreamID":100}]}`, 100),
		}
		err := VerifyChannelDefinitionsForAdmission(codecs, channelDefs, map[llotypes.ChannelID]struct{}{2: {}})
		require.EqualError(t, err, "ChannelDefinition with ID 2 declares calculated stream 100 already declared by channel 1")
	})
	t.Run("ignores tombstoned channels", func(t *testing.T) {
		tombstoned := exprChannel(11, `{"abi":[{"expressionStreamID":100}]}`, 100)
		tombstoned.Tombstone = true
		channelDefs := llotypes.ChannelDefinitions{
			1: exprChannel(10, `{"abi":[{"expressionStreamID":100}]}`, 100),
			2: tombstoned,
		}
		require.NoError(t, verifyAdmittingAll(codecs, channelDefs))
	})
}

type mockFeedIDCodec struct {
	mockReportCodec
	feedID [32]byte
	ok     bool
	err    error
}

func (m mockFeedIDCodec) FeedID(llotypes.ChannelDefinition) ([32]byte, bool, error) {
	return m.feedID, m.ok, m.err
}

func Test_VerifyChannelDefinitions_FeedIDCollisions(t *testing.T) {
	mockReportFormat := llotypes.ReportFormat(0)
	channel := func() llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: mockReportFormat,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
		}
	}
	// The mock returns the same feed ID for every channel, so any definitions
	// set with more than one channel collides.
	codecs := func(c ReportCodec) map[llotypes.ReportFormat]ReportCodec {
		return map[llotypes.ReportFormat]ReportCodec{mockReportFormat: c}
	}

	t.Run("accepts a single channel with a feed ID", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{1: channel()}
		require.NoError(t, verifyAdmittingAll(codecs(mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true}), channelDefs))
	})
	t.Run("fails when two channels share a feed ID", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{1: channel(), 2: channel()}
		err := verifyAdmittingAll(codecs(mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true}), channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 2 has feed ID 0xab00000000000000000000000000000000000000000000000000000000000000 already used by channel 1")
	})
	t.Run("ignores channels that report no feed ID", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{1: channel(), 2: channel()}
		require.NoError(t, verifyAdmittingAll(codecs(mockFeedIDCodec{ok: false}), channelDefs))
	})
	t.Run("fails when the feed ID cannot be resolved", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{1: channel()}
		err := verifyAdmittingAll(codecs(mockFeedIDCodec{err: errors.New("bad opts")}), channelDefs)
		require.EqualError(t, err, "invalid ChannelDefinition with ID 1: failed to resolve feed ID: bad opts")
	})
	t.Run("does not resolve the feed ID of a channel Verify rejected", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{1: channel(), 2: channel()}
		codec := mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true}
		codec.mockReportCodec = mockReportCodec{err: errors.New("rejected")}
		err := verifyAdmittingAll(codecs(codec), channelDefs)
		// Only the two Verify errors: no feed ID was collected, so no collision.
		require.EqualError(t, err, "invalid ChannelDefinition with ID 1: rejected\ninvalid ChannelDefinition with ID 2: rejected")
	})
	t.Run("ignores tombstoned channels", func(t *testing.T) {
		tombstoned := channel()
		tombstoned.Tombstone = true
		channelDefs := llotypes.ChannelDefinitions{1: channel(), 2: tombstoned}
		require.NoError(t, verifyAdmittingAll(codecs(mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true}), channelDefs))
	})
}

// mockAdmissionCodec rejects every definition on admission, and accepts every
// definition otherwise.
type mockAdmissionCodec struct{ mockReportCodec }

func (mockAdmissionCodec) VerifyForAdmission(llotypes.ChannelDefinition) error {
	return errors.New("admission rejected")
}

func Test_VerifyChannelDefinitions_AdmissionScope(t *testing.T) {
	feedIDCodecs := map[llotypes.ReportFormat]ReportCodec{
		0: mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true},
	}
	channel := func() llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}}
	}
	colliding := llotypes.ChannelDefinitions{1: channel(), 2: channel()}

	t.Run("committed definitions are not held to the admission-only checks", func(t *testing.T) {
		// Both channels share a feed ID, which would be rejected on admission.
		// Neither is being admitted, so verification passes and the oracles keep
		// observing rather than halting.
		require.NoError(t, VerifyChannelDefinitions(feedIDCodecs, colliding))
		require.NoError(t, VerifyChannelDefinitionsForAdmission(feedIDCodecs, colliding, nil))
		require.NoError(t, VerifyChannelDefinitionsForAdmission(feedIDCodecs, colliding, map[llotypes.ChannelID]struct{}{}))
	})
	t.Run("a cross-definition finding is kept when either channel is admitted", func(t *testing.T) {
		// The finding is reported against channel 2, the one seen second, so
		// admitting only channel 1 must still keep it -- otherwise a new channel
		// could squat the feed ID of a committed one with a higher ID.
		for _, admitted := range []llotypes.ChannelID{1, 2} {
			err := VerifyChannelDefinitionsForAdmission(feedIDCodecs, colliding, map[llotypes.ChannelID]struct{}{admitted: {}})
			require.EqualError(t, err, "ChannelDefinition with ID 2 has feed ID 0xab00000000000000000000000000000000000000000000000000000000000000 already used by channel 1")
		}
	})
	t.Run("AdmissionVerifier is only consulted for admitted channels", func(t *testing.T) {
		codecs := map[llotypes.ReportFormat]ReportCodec{0: mockAdmissionCodec{}}
		defs := llotypes.ChannelDefinitions{1: channel(), 2: channel()}
		require.NoError(t, VerifyChannelDefinitions(codecs, defs))
		err := VerifyChannelDefinitionsForAdmission(codecs, defs, map[llotypes.ChannelID]struct{}{2: {}})
		require.EqualError(t, err, "invalid ChannelDefinition with ID 2: admission rejected")
	})
	t.Run("baseline checks apply to committed definitions", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: {}}
		require.EqualError(t, VerifyChannelDefinitions(feedIDCodecs, defs), "ChannelDefinition with ID 1 has no streams")
	})
}

func Test_InadmissibleChannelIDs(t *testing.T) {
	codecs := map[llotypes.ReportFormat]ReportCodec{0: stubReportCodec{}}
	feedIDCodecs := map[llotypes.ReportFormat]ReportCodec{0: mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true}}
	channel := func(streamID llotypes.StreamID) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{Streams: []llotypes.Stream{{StreamID: streamID, Aggregator: llotypes.AggregatorMedian}}}
	}
	ids := func(channelIDs ...llotypes.ChannelID) map[llotypes.ChannelID]struct{} {
		set := make(map[llotypes.ChannelID]struct{}, len(channelIDs))
		for _, channelID := range channelIDs {
			set[channelID] = struct{}{}
		}
		return set
	}

	t.Run("attributes findings to the admitted channels that produced them", func(t *testing.T) {
		unknownAggregator := channel(2)
		unknownAggregator.Streams[0].Aggregator = 99
		defs := llotypes.ChannelDefinitions{1: channel(1), 2: unknownAggregator, 3: {}}
		got, err := InadmissibleChannelIDsWithCache(codecs, defs, ids(1, 2, 3), nil)
		require.NoError(t, err)
		require.Equal(t, ids(2, 3), got)
	})
	t.Run("leaves out findings against committed channels", func(t *testing.T) {
		// Channel 1 fails a baseline check but is not being admitted.
		defs := llotypes.ChannelDefinitions{1: {}, 2: channel(2)}
		got, err := InadmissibleChannelIDsWithCache(codecs, defs, ids(2), nil)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	t.Run("a cross-definition finding implicates only the admitted side", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{1: channel(1), 2: channel(1)}
		for _, admitted := range []llotypes.ChannelID{1, 2} {
			got, err := InadmissibleChannelIDsWithCache(feedIDCodecs, defs, ids(admitted), nil)
			require.NoError(t, err)
			require.Equal(t, ids(admitted), got)
		}
	})
	t.Run("a whole-set budget is an error only when something is admitted", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{}
		for c := range MaxTotalOptsBytes/MaxChannelOptsBytes + 1 {
			cd := channel(1)
			cd.Opts = []byte(strings.Repeat("x", MaxChannelOptsBytes))
			defs[llotypes.ChannelID(c+1)] = cd
		}
		got, err := InadmissibleChannelIDsWithCache(codecs, defs, ids(1), nil)
		require.Error(t, err)
		require.Empty(t, got)

		got, err = InadmissibleChannelIDsWithCache(codecs, defs, ids(), nil)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	t.Run("too many channels is an error", func(t *testing.T) {
		defs := llotypes.ChannelDefinitions{}
		for c := range MaxOutcomeChannelDefinitionsLength + 1 {
			defs[llotypes.ChannelID(c+1)] = channel(1)
		}
		_, err := InadmissibleChannelIDsWithCache(codecs, defs, ids(1), nil)
		require.EqualError(t, err, fmt.Sprintf("too many channels, got: %d/%d", MaxOutcomeChannelDefinitionsLength+1, MaxOutcomeChannelDefinitionsLength))
	})
}

func Test_VerifyChannelDefinitions_SizeBudgets(t *testing.T) {
	codecs := map[llotypes.ReportFormat]ReportCodec{0: stubReportCodec{}}

	channel := func(streams int, optsBytes int) llotypes.ChannelDefinition {
		cd := llotypes.ChannelDefinition{Streams: make([]llotypes.Stream, 0, streams)}
		for i := range streams {
			cd.Streams = append(cd.Streams, llotypes.Stream{StreamID: llotypes.StreamID(i + 1), Aggregator: llotypes.AggregatorMedian})
		}
		if optsBytes > 0 {
			cd.Opts = []byte(strings.Repeat("x", optsBytes))
		}
		return cd
	}
	// Channels sharing one stream ID, so the whole-set entry count grows while
	// the unique-stream-ID cap stays untouched. That is the case the per-channel
	// and per-set caps miss between them.
	sharedStream := func(channels, streamsEach int) llotypes.ChannelDefinitions {
		defs := llotypes.ChannelDefinitions{}
		for c := range channels {
			cd := llotypes.ChannelDefinition{Streams: make([]llotypes.Stream, 0, streamsEach)}
			for i := range streamsEach {
				cd.Streams = append(cd.Streams, llotypes.Stream{StreamID: llotypes.StreamID(i + 1), Aggregator: llotypes.AggregatorMedian})
			}
			defs[llotypes.ChannelID(c+1)] = cd
		}
		return defs
	}

	t.Run("per-channel opts length", func(t *testing.T) {
		atLimit := llotypes.ChannelDefinitions{1: channel(1, MaxChannelOptsBytes)}
		require.NoError(t, verifyAdmittingAll(codecs, atLimit))

		over := llotypes.ChannelDefinitions{1: channel(1, MaxChannelOptsBytes+1)}
		require.EqualError(t, verifyAdmittingAll(codecs, over),
			fmt.Sprintf("ChannelDefinition with ID 1 has opts that are too long, got: %d/%d", MaxChannelOptsBytes+1, MaxChannelOptsBytes))
	})

	t.Run("total stream entries across the set", func(t *testing.T) {
		// Ten channels listing the same 5_000 streams: 50_000 entries against
		// 5_000 unique IDs, and every channel well under MaxStreamsPerChannel.
		atLimit := sharedStream(10, MaxTotalStreamEntries/10)
		require.NoError(t, verifyAdmittingAll(codecs, atLimit))

		over := sharedStream(11, MaxTotalStreamEntries/10)
		require.EqualError(t, verifyAdmittingAll(codecs, over),
			fmt.Sprintf("too many stream entries across all channels, got: %d/%d", 11*(MaxTotalStreamEntries/10), MaxTotalStreamEntries))
	})

	t.Run("aggregated pairs across the set", func(t *testing.T) {
		// One channel per aggregator, each listing the same streams, so the
		// pair count grows while the unique-stream-ID cap and the entry budget
		// stay untouched.
		perAggregator := func(aggregators ...llotypes.Aggregator) llotypes.ChannelDefinitions {
			defs := llotypes.ChannelDefinitions{}
			for c, agg := range aggregators {
				cd := llotypes.ChannelDefinition{Streams: make([]llotypes.Stream, 0, MaxObservationStreamValuesLength)}
				for i := range MaxObservationStreamValuesLength {
					cd.Streams = append(cd.Streams, llotypes.Stream{StreamID: llotypes.StreamID(i + 1), Aggregator: agg})
				}
				defs[llotypes.ChannelID(c+1)] = cd
			}
			return defs
		}

		atLimit := perAggregator(llotypes.AggregatorMedian, llotypes.AggregatorMode)
		require.NoError(t, verifyAdmittingAll(codecs, atLimit))

		over := perAggregator(llotypes.AggregatorMedian, llotypes.AggregatorMode, llotypes.AggregatorQuote)
		require.EqualError(t, verifyAdmittingAll(codecs, over),
			fmt.Sprintf("too many aggregated (stream, aggregator) pairs across all channels, got: %d/%d", 3*MaxObservationStreamValuesLength, MaxPersistedAggregates))
	})

	t.Run("a pair costs one aggregate however many channels name it", func(t *testing.T) {
		// The same (stream, aggregator) pair repeated across channels is one
		// carried-forward value, so the pair budget must count it once even
		// where the entry budget counts it every time.
		defs := sharedStream(4, MaxPersistedAggregates/2)
		require.NoError(t, verifyAdmittingAll(codecs, defs))
	})

	t.Run("calculated streams across the set", func(t *testing.T) {
		// Expression channels declaring distinct calculated stream IDs, spread
		// across enough channels to stay under MaxChannelOptsBytes. The entry
		// and opts budgets stay well clear: a calculated stream costs an ABI
		// entry, not a stream entry.
		const perChannel = 500
		calculated := func(total int) llotypes.ChannelDefinitions {
			defs := llotypes.ChannelDefinitions{}
			next := llotypes.StreamID(1_000_000)
			for c := 0; total > 0; c++ {
				n := min(perChannel, total)
				total -= n
				abi := make([]string, 0, n)
				for range n {
					abi = append(abi, fmt.Sprintf(`{"expressionStreamID":%d}`, next))
					next++
				}
				defs[llotypes.ChannelID(c+1)] = llotypes.ChannelDefinition{
					ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
					Streams:      []llotypes.Stream{{StreamID: llotypes.StreamID(c + 1), Aggregator: llotypes.AggregatorMedian}},
					Opts:         llotypes.ChannelOpts(fmt.Sprintf(`{"abi":[%s]}`, strings.Join(abi, ","))),
				}
			}
			return defs
		}

		atLimit := calculated(MaxTotalCalculatedStreams)
		require.NoError(t, verifyAdmittingAll(codecs, atLimit))

		over := calculated(MaxTotalCalculatedStreams + 1)
		require.EqualError(t, verifyAdmittingAll(codecs, over),
			fmt.Sprintf("too many calculated streams across all channels, got: %d/%d", MaxTotalCalculatedStreams+1, MaxTotalCalculatedStreams))
	})

	t.Run("total opts bytes across the set", func(t *testing.T) {
		// Every channel individually within MaxChannelOptsBytes; only the sum
		// exceeds the budget.
		set := func(channels int) llotypes.ChannelDefinitions {
			defs := llotypes.ChannelDefinitions{}
			for c := range channels {
				defs[llotypes.ChannelID(c+1)] = channel(1, MaxChannelOptsBytes)
			}
			return defs
		}
		atLimit := set(MaxTotalOptsBytes / MaxChannelOptsBytes)
		require.NoError(t, verifyAdmittingAll(codecs, atLimit))

		over := set(MaxTotalOptsBytes/MaxChannelOptsBytes + 1)
		require.EqualError(t, verifyAdmittingAll(codecs, over),
			fmt.Sprintf("too many opts bytes across all channels, got: %d/%d", MaxTotalOptsBytes+MaxChannelOptsBytes, MaxTotalOptsBytes))
	})

	t.Run("TWAP calls across the set", func(t *testing.T) {
		twapCodecs := map[llotypes.ReportFormat]ReportCodec{0: twapCodec{}}
		set := func(channels int) llotypes.ChannelDefinitions {
			defs := llotypes.ChannelDefinitions{}
			for c := range channels {
				defs[llotypes.ChannelID(c+1)] = channel(1, 0)
			}
			return defs
		}
		perChannel := MaxTWAPCallsPerExpression
		atLimit := set(MaxTotalTWAPCalls / perChannel)
		require.NoError(t, verifyAdmittingAll(twapCodecs, atLimit))

		over := set(MaxTotalTWAPCalls/perChannel + 1)
		require.EqualError(t, verifyAdmittingAll(twapCodecs, over),
			fmt.Sprintf("too many TWAP calls across all channels, got: %d/%d", MaxTotalTWAPCalls+perChannel, MaxTotalTWAPCalls))
		// Admission-only, like the other budgets.
		require.NoError(t, VerifyChannelDefinitions(twapCodecs, over))

		// A tombstone is never evaluated, so it does no TWAP work.
		tombstoned := over[1]
		tombstoned.Tombstone = true
		over[1] = tombstoned
		require.NoError(t, verifyAdmittingAll(twapCodecs, over))
	})

	t.Run("budgets are admission-only", func(t *testing.T) {
		// A committed set over budget must not fail verification: every oracle
		// runs this every round, so rejecting it would halt the protocol over
		// definitions that are already installed.
		over := llotypes.ChannelDefinitions{1: channel(1, MaxChannelOptsBytes+1)}
		require.NoError(t, VerifyChannelDefinitions(codecs, over))
		require.NoError(t, VerifyChannelDefinitionsForAdmission(codecs, over, nil))
		require.NoError(t, VerifyChannelDefinitionsForAdmission(codecs, over, map[llotypes.ChannelID]struct{}{}))
	})

	t.Run("a whole-set finding applies to whatever is being admitted", func(t *testing.T) {
		// The budget is a property of the set, so the channels that pushed it
		// over are not distinguishable. Admitting any channel is refused while
		// the set is over budget, which stops it growing; admitting nothing is
		// left alone.
		over := sharedStream(11, MaxTotalStreamEntries/10)
		for _, admitted := range []llotypes.ChannelID{1, 11} {
			err := VerifyChannelDefinitionsForAdmission(codecs, over, map[llotypes.ChannelID]struct{}{admitted: {}})
			require.ErrorContains(t, err, "too many stream entries across all channels")
		}
	})

	t.Run("tombstones count toward the budgets", func(t *testing.T) {
		// A tombstone is persisted in the definitions record like any other
		// definition, so its streams and opts consume the same budgets.
		defs := sharedStream(11, MaxTotalStreamEntries/10)
		tombstoned := defs[11]
		tombstoned.Tombstone = true
		defs[11] = tombstoned
		require.EqualError(t, verifyAdmittingAll(codecs, defs),
			fmt.Sprintf("too many stream entries across all channels, got: %d/%d", 11*(MaxTotalStreamEntries/10), MaxTotalStreamEntries))

		over := channel(1, MaxChannelOptsBytes+1)
		over.Tombstone = true
		require.EqualError(t, verifyAdmittingAll(codecs, llotypes.ChannelDefinitions{1: over}),
			fmt.Sprintf("ChannelDefinition with ID 1 has opts that are too long, got: %d/%d", MaxChannelOptsBytes+1, MaxChannelOptsBytes))

		set := llotypes.ChannelDefinitions{}
		for c := range MaxTotalOptsBytes/MaxChannelOptsBytes + 1 {
			cd := channel(1, MaxChannelOptsBytes)
			cd.Tombstone = c == 0
			set[llotypes.ChannelID(c+1)] = cd
		}
		require.EqualError(t, verifyAdmittingAll(codecs, set),
			fmt.Sprintf("too many opts bytes across all channels, got: %d/%d", MaxTotalOptsBytes+MaxChannelOptsBytes, MaxTotalOptsBytes))

		manyStreams := channel(MaxStreamsPerChannel+1, 0)
		manyStreams.Tombstone = true
		require.EqualError(t, verifyAdmittingAll(codecs, llotypes.ChannelDefinitions{1: manyStreams}),
			fmt.Sprintf("ChannelDefinition with ID 1 has too many streams, got: %d/%d", MaxStreamsPerChannel+1, MaxStreamsPerChannel))
		// Admission-only: a committed tombstone over the limits still verifies.
		require.NoError(t, VerifyChannelDefinitions(codecs, llotypes.ChannelDefinitions{1: manyStreams}))
	})
}

func Test_ChangedChannelIDs(t *testing.T) {
	streams := []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}
	current := llotypes.ChannelDefinitions{
		1: {Streams: streams},
		2: {Streams: streams},
		3: {Streams: streams},
	}
	desired := llotypes.ChannelDefinitions{
		1: {Streams: streams},                                         // unchanged
		2: {Streams: streams, ReportFormat: llotypes.ReportFormat(7)}, // changed
		4: {Streams: streams},                                         // added
	}
	// Channel 3 is only being removed, so it is not being admitted.
	require.Equal(t, map[llotypes.ChannelID]struct{}{2: {}, 4: {}}, ChangedChannelIDs(current, desired))
	require.Empty(t, ChangedChannelIDs(current, current))
	require.Empty(t, ChangedChannelIDs(current, nil))
}

// rejectingCodec fails Verify for the channels whose IDs are in reject, keyed
// by the first stream ID of the definition (channel IDs are not passed to
// Verify).
type rejectingCodec struct {
	rejectStream llotypes.StreamID
}

func (rejectingCodec) Encode(Report, llotypes.ChannelDefinition, *OptsCache) ([]byte, error) {
	return nil, nil
}

func (c rejectingCodec) Verify(cd llotypes.ChannelDefinition) error {
	if len(cd.Streams) > 0 && cd.Streams[0].StreamID == c.rejectStream {
		return errors.New("codec says no")
	}
	return nil
}

func Test_UnverifiableChannelIDs(t *testing.T) {
	channel := func(streamID llotypes.StreamID) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{Streams: []llotypes.Stream{{StreamID: streamID, Aggregator: llotypes.AggregatorMedian}}}
	}

	t.Run("attributes a baseline failure to exactly its channel", func(t *testing.T) {
		codecs := map[llotypes.ReportFormat]ReportCodec{0: rejectingCodec{rejectStream: 2}}
		defs := llotypes.ChannelDefinitions{1: channel(1), 2: channel(2), 3: channel(3)}

		ids, err := UnverifiableChannelIDs(codecs, defs)
		require.NoError(t, err)
		require.Equal(t, map[llotypes.ChannelID]struct{}{2: {}}, ids)

		// The same set is still a hard error on the admission path.
		require.EqualError(t, VerifyChannelDefinitions(codecs, defs), "invalid ChannelDefinition with ID 2: codec says no")
	})

	t.Run("attributes every kind of baseline finding", func(t *testing.T) {
		codecs := map[llotypes.ReportFormat]ReportCodec{0: stubReportCodec{}}
		tooManyStreams := llotypes.ChannelDefinition{Streams: make([]llotypes.Stream, MaxStreamsPerChannel+1)}
		for i := range tooManyStreams.Streams {
			tooManyStreams.Streams[i] = llotypes.Stream{StreamID: llotypes.StreamID(i + 100), Aggregator: llotypes.AggregatorMedian}
		}
		defs := llotypes.ChannelDefinitions{
			1: channel(1),
			2: {},                                          // no streams
			3: {Streams: []llotypes.Stream{{StreamID: 3}}}, // zero aggregator
			4: tooManyStreams,
		}

		ids, err := UnverifiableChannelIDs(codecs, defs)
		require.NoError(t, err)
		require.Equal(t, map[llotypes.ChannelID]struct{}{2: {}, 3: {}, 4: {}}, ids)
	})

	t.Run("admission-only findings are not attributed", func(t *testing.T) {
		// Two channels sharing a feed ID is admission-only: a committed set
		// carrying it keeps reporting, so neither channel is skipped.
		codecs := map[llotypes.ReportFormat]ReportCodec{0: mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true}}
		defs := llotypes.ChannelDefinitions{1: channel(1), 2: channel(2)}

		ids, err := UnverifiableChannelIDs(codecs, defs)
		require.NoError(t, err)
		require.Empty(t, ids)
		require.Error(t, verifyAdmittingAll(codecs, defs))
	})

	t.Run("a tombstone is never unverifiable", func(t *testing.T) {
		codecs := map[llotypes.ReportFormat]ReportCodec{0: rejectingCodec{rejectStream: 2}}
		defs := llotypes.ChannelDefinitions{1: channel(1), 2: {Tombstone: true}}

		ids, err := UnverifiableChannelIDs(codecs, defs)
		require.NoError(t, err)
		require.Empty(t, ids)
	})

	t.Run("a whole-set finding is returned as an error with no channels to skip", func(t *testing.T) {
		codecs := map[llotypes.ReportFormat]ReportCodec{0: stubReportCodec{}}
		defs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength+1)
		for i := range MaxOutcomeChannelDefinitionsLength + 1 {
			defs[llotypes.ChannelID(i+1)] = channel(llotypes.StreamID(i + 1))
		}

		ids, err := UnverifiableChannelIDs(codecs, defs)
		require.Error(t, err)
		require.Contains(t, err.Error(), "too many channels")
		require.Empty(t, ids)
	})

	t.Run("an empty definition set is clean", func(t *testing.T) {
		ids, err := UnverifiableChannelIDs(map[llotypes.ReportFormat]ReportCodec{}, llotypes.ChannelDefinitions{})
		require.NoError(t, err)
		require.Empty(t, ids)
	})
}

// analyzeBaseline must report exactly the baseline findings of
// analyzeChannelDefinitions, attributed the same way, and none of its
// admission-only findings.
func Test_AnalyzeBaseline_MatchesFullAnalysis(t *testing.T) {
	stream := func(streamID llotypes.StreamID) llotypes.Stream {
		return llotypes.Stream{StreamID: streamID, Aggregator: llotypes.AggregatorMedian}
	}
	channel := func(streams ...llotypes.Stream) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{Streams: streams}
	}
	tooManyStreams := llotypes.ChannelDefinition{Streams: make([]llotypes.Stream, MaxStreamsPerChannel+1)}
	for i := range tooManyStreams.Streams {
		tooManyStreams.Streams[i] = stream(llotypes.StreamID(i + 100_000))
	}
	backfill := func(target llotypes.ChannelID) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Streams:      []llotypes.Stream{stream(1)},
			Opts:         fmt.Appendf(nil, `{"targetChannelId":%d,"observations":{"1000":{"1":"1"}}}`, target),
		}
	}
	tombstone := channel(stream(9))
	tombstone.Tombstone = true
	oversizedOpts := channel(stream(10))
	oversizedOpts.Opts = make([]byte, MaxChannelOptsBytes+1)
	unknownAggregator := channel(llotypes.Stream{StreamID: 11, Aggregator: llotypes.Aggregator(250)})

	rejecting := rejectingCodec{rejectStream: 2}
	sharedFeedID := mockFeedIDCodec{feedID: [32]byte{0xab}, ok: true}
	sharedFeedID.mockReportCodec = mockReportCodec{}

	tooManyChannels := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength+1)
	for i := range llotypes.ChannelID(MaxOutcomeChannelDefinitionsLength + 1) {
		tooManyChannels[i] = channel(stream(1))
	}
	// Within the channel limit, but with more unique stream IDs than allowed, so
	// the stream ID count is what both analyses report.
	const streamsPerChannel = MaxObservationStreamValuesLength/MaxOutcomeChannelDefinitionsLength + 1
	tooManyStreamIDs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength)
	for i := range llotypes.ChannelID(MaxOutcomeChannelDefinitionsLength) {
		cd := channel()
		for k := range llotypes.StreamID(streamsPerChannel) {
			cd.Streams = append(cd.Streams, stream(llotypes.StreamID(i)*streamsPerChannel+k))
		}
		tooManyStreamIDs[i] = cd
	}
	require.Greater(t, analyzeBaseline(map[llotypes.ReportFormat]ReportCodec{0: mockReportCodec{}}, tooManyStreamIDs, nil).uniqueStreamIDs, MaxObservationStreamValuesLength)

	for _, tc := range []struct {
		name   string
		codecs map[llotypes.ReportFormat]ReportCodec
		defs   llotypes.ChannelDefinitions
		// admissionOnly pins that the full analysis does find admission-only
		// findings, which the baseline must leave out.
		admissionOnly bool
	}{
		{
			name:          "every kind of per channel finding",
			admissionOnly: true,
			codecs: map[llotypes.ReportFormat]ReportCodec{
				0:                                    rejecting,
				llotypes.ReportFormatHistoryBackfill: ReportCodecHistoryBackfill{},
			},
			defs: llotypes.ChannelDefinitions{
				1:  channel(stream(1)),
				2:  channel(stream(2)),                               // Verify rejects
				3:  {},                                               // no streams
				4:  channel(llotypes.Stream{StreamID: 4}, stream(2)), // zero aggregator and Verify rejects
				5:  tooManyStreams,
				6:  tombstone,
				7:  oversizedOpts,     // admission-only
				8:  unknownAggregator, // admission-only
				20: backfill(99),      // target missing
				21: backfill(1),
			},
		},
		{
			name:          "admission-only feed ID collision",
			admissionOnly: true,
			codecs:        map[llotypes.ReportFormat]ReportCodec{0: sharedFeedID},
			defs:          llotypes.ChannelDefinitions{1: channel(stream(1)), 2: channel(stream(2))},
		},
		{
			name:          "admission-only rejection",
			admissionOnly: true,
			codecs:        map[llotypes.ReportFormat]ReportCodec{0: mockAdmissionCodec{}},
			defs:          llotypes.ChannelDefinitions{1: channel(stream(1))},
		},
		{
			name:   "too many channels",
			codecs: map[llotypes.ReportFormat]ReportCodec{0: mockReportCodec{}},
			defs:   tooManyChannels,
		},
		{
			name:   "too many unique stream IDs",
			codecs: map[llotypes.ReportFormat]ReportCodec{0: mockReportCodec{}},
			defs:   tooManyStreamIDs,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := analyzeChannelDefinitions(tc.codecs, tc.defs, nil)
			baseline := analyzeBaseline(tc.codecs, tc.defs, nil)

			require.Equal(t, tc.admissionOnly, len(full.admissionFindings) > 0)
			requireSameError(t, full.err(nil), baseline.err(nil))
			requireSameError(t, full.setErr(), baseline.setErr())
			requireSameError(t, full.wholeSetErr, baseline.wholeSetErr)
			require.Equal(t, full.uniqueStreamIDs, baseline.uniqueStreamIDs)
			require.Len(t, baseline.channelErrs, len(full.channelErrs))
			for channelID, err := range full.channelErrs {
				requireSameError(t, err, baseline.channelErrs[channelID])
			}
			require.Empty(t, baseline.admissionFindings)
		})
	}
}

// A baseline analysis through a read-only view runs Verify alone on a miss and
// records nothing. Through a cache that records, a miss records complete facts,
// so the admission path and the state transition budget can read them later.
func Test_AnalyzeBaseline_Cache(t *testing.T) {
	defs := llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`), 2: cacheTestDef(`{"a":2}`)}

	t.Run("a read-only view runs Verify alone and records nothing", func(t *testing.T) {
		codec := &countingReportCodec{}
		cache := NewChannelAnalysisCache()

		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, cache.ReadOnly()))
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, cache.ReadOnly()))
		verifies, admissions, feedIDs := codec.calls()
		require.Equal(t, int64(4), verifies)
		require.Zero(t, admissions)
		require.Zero(t, feedIDs)
	})

	t.Run("a cache that records holds complete facts after a miss", func(t *testing.T) {
		codec := &countingReportCodec{}
		cache := NewChannelAnalysisCache()

		ids, err := UnverifiableChannelIDsWithCache(countingCodecs(codec), defs, cache)
		require.NoError(t, err)
		require.Empty(t, ids)
		verifies, admissions, feedIDs := codec.calls()
		require.Equal(t, []int64{2, 2, 2}, []int64{verifies, admissions, feedIDs})

		// Admission and a read-only baseline both hit.
		require.NoError(t, VerifyChannelDefinitionsForAdmissionWithCache(countingCodecs(codec), defs, map[llotypes.ChannelID]struct{}{1: {}}, cache))
		require.NoError(t, VerifyChannelDefinitionsWithCache(countingCodecs(codec), defs, cache.ReadOnly()))
		verifies, admissions, feedIDs = codec.calls()
		require.Equal(t, []int64{2, 2, 2}, []int64{verifies, admissions, feedIDs})
	})

	t.Run("a cached Verify error is served from the cache", func(t *testing.T) {
		failing := &countingReportCodec{verifyErr: errors.New("bad opts")}
		cache := NewChannelAnalysisCache()

		ids, err := UnverifiableChannelIDsWithCache(countingCodecs(failing), defs, cache)
		require.NoError(t, err)
		require.Equal(t, map[llotypes.ChannelID]struct{}{1: {}, 2: {}}, ids)
		require.EqualError(t, VerifyChannelDefinitionsWithCache(countingCodecs(failing), defs, cache.ReadOnly()),
			"invalid ChannelDefinition with ID 1: bad opts\ninvalid ChannelDefinition with ID 2: bad opts")
		verifies, _, _ := failing.calls()
		require.Equal(t, int64(2), verifies)
	})
}

// With nothing being admitted, the admission entry point gives the baseline
// verdict and computes no admission-only finding.
func Test_VerifyChannelDefinitionsForAdmission_NothingAdmitted(t *testing.T) {
	codec := &countingReportCodec{admissionErr: errors.New("not admissible")}
	defs := llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)}

	require.NoError(t, VerifyChannelDefinitionsForAdmission(countingCodecs(codec), defs, map[llotypes.ChannelID]struct{}{}))
	require.NoError(t, VerifyChannelDefinitionsForAdmission(countingCodecs(codec), defs, nil))
	_, admissions, feedIDs := codec.calls()
	require.Zero(t, admissions)
	require.Zero(t, feedIDs)

	require.EqualError(t, VerifyChannelDefinitionsForAdmission(countingCodecs(codec), defs, map[llotypes.ChannelID]struct{}{1: {}}),
		"invalid ChannelDefinition with ID 1: not admissible")
}

// With nothing being admitted, InadmissibleChannelIDsWithCache gives the same
// answer as the full analysis and computes no admission-only finding.
func Test_InadmissibleChannelIDs_NothingAdmitted(t *testing.T) {
	stream := llotypes.Stream{StreamID: 1, Aggregator: llotypes.AggregatorMedian}
	tooManyChannels := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength+1)
	for i := range llotypes.ChannelID(MaxOutcomeChannelDefinitionsLength + 1) {
		tooManyChannels[i] = llotypes.ChannelDefinition{Streams: []llotypes.Stream{stream}}
	}

	for _, tc := range []struct {
		name  string
		codec *countingReportCodec
		defs  llotypes.ChannelDefinitions
	}{
		{"valid", &countingReportCodec{}, llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`)}},
		{"baseline and admission failures", &countingReportCodec{verifyErr: errors.New("bad opts"), admissionErr: errors.New("not admissible")}, llotypes.ChannelDefinitions{1: cacheTestDef(`{"a":1}`), 2: {}}},
		{"too many channels", &countingReportCodec{}, tooManyChannels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codecs := countingCodecs(tc.codec)
			res := analyzeChannelDefinitions(codecs, tc.defs, nil)
			var wantIDs map[llotypes.ChannelID]struct{}
			if res.wholeSetErr == nil {
				wantIDs = map[llotypes.ChannelID]struct{}{}
			}
			_, wantAdmissions, wantFeedIDs := tc.codec.calls()

			for _, admitting := range []map[llotypes.ChannelID]struct{}{nil, {}} {
				ids, err := InadmissibleChannelIDsWithCache(codecs, tc.defs, admitting, NewChannelAnalysisCache().ReadOnly())
				requireSameError(t, res.wholeSetErr, err)
				require.Equal(t, wantIDs, ids)
			}
			_, admissions, feedIDs := tc.codec.calls()
			require.Equal(t, wantAdmissions, admissions, "no admission check runs")
			require.Equal(t, wantFeedIDs, feedIDs)
		})
	}
}
