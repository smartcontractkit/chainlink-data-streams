package protocol

import (
	"errors"
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
			`not json`:                           "invalid ChannelDefinition with ID 1: failed to decode calculated stream opts: invalid character 'o' in literal null (expecting 'u')",
			`{}`:                                 "invalid ChannelDefinition with ID 1: no expressions found in channel definition",
			`{"abi":[{"expressionStreamID":0}]}`: "invalid ChannelDefinition with ID 1: expression stream ID is 0, abi index: 0",
		} {
			channelDefs := llotypes.ChannelDefinitions{1: exprChannel(10, opts)}
			require.EqualError(t, verifyAdmittingAll(codecs, channelDefs), want)
		}
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
