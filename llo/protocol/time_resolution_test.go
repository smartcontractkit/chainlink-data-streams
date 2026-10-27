package protocol

import (
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_ReportTimeResolution_ClassifiesEveryReportFormat(t *testing.T) {
	// Every format chainlink-common defines must be classified here on
	// purpose. A format missing from this table is a codec whose truncation
	// the reportability guard would otherwise skip, so the test fails until
	// someone maps it in ReportTimeResolution and records the decision below.
	type want struct {
		res TimeResolution
		err string
	}
	table := map[llotypes.ReportFormat]want{
		llotypes.ReportFormatEVMPremiumLegacy:         {res: ResolutionSeconds},
		llotypes.ReportFormatEVMABIEncodeUnpacked:     {res: ResolutionSeconds},
		llotypes.ReportFormatEVMABIEncodeUnpackedExpr: {res: ResolutionSeconds},
		llotypes.ReportFormatJSON:                     {res: ResolutionNanoseconds},
		llotypes.ReportFormatEVMStreamlined:           {res: ResolutionNanoseconds},
		llotypes.ReportFormatHistoryBackfill:          {err: "encoded with the target channel's codec"},
		llotypes.ReportFormatRetirement:               {err: "has no report codec"},
		llotypes.ReportFormatCapabilityTrigger:        {err: "has no report codec"},
	}
	for _, rf := range llotypes.ReportFormats {
		t.Run(rf.String(), func(t *testing.T) {
			w, ok := table[rf]
			require.True(t, ok, "report format %s is not classified: add it to ReportTimeResolution and to this table", rf)
			res, err := ReportTimeResolution(nil, llotypes.ChannelDefinition{ReportFormat: rf}, 1)
			if w.err != "" {
				require.ErrorContains(t, err, w.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, w.res, res)
		})
	}

	_, err := ReportTimeResolution(nil, llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormat(1000)}, 1)
	require.ErrorContains(t, err, "has no time resolution mapping")
}

func Test_ReportTimeResolution_ReadsOpts(t *testing.T) {
	for _, rf := range []llotypes.ReportFormat{llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.ReportFormatEVMABIEncodeUnpackedExpr} {
		for _, tc := range []struct {
			name string
			opts string
			want TimeResolution
		}{
			{"unset defaults to seconds", `{"abi":[]}`, ResolutionSeconds},
			{"seconds", `{"TimeResolution":"s"}`, ResolutionSeconds},
			{"milliseconds", `{"TimeResolution":"ms"}`, ResolutionMilliseconds},
			{"microseconds", `{"TimeResolution":"us"}`, ResolutionMicroseconds},
			{"nanoseconds", `{"TimeResolution":"ns"}`, ResolutionNanoseconds},
		} {
			t.Run(rf.String()+" "+tc.name, func(t *testing.T) {
				cd := llotypes.ChannelDefinition{ReportFormat: rf, Opts: []byte(tc.opts)}

				res, err := ReportTimeResolution(nil, cd, 1)
				require.NoError(t, err)
				require.Equal(t, tc.want, res, "decoded from raw opts")

				cache := NewOptsCache()
				cache.Set(1, cd.Opts)
				res, err = ReportTimeResolution(cache, cd, 1)
				require.NoError(t, err)
				require.Equal(t, tc.want, res, "read through the opts cache")
			})
		}

		t.Run(rf.String()+" undecodable opts", func(t *testing.T) {
			for _, opts := range []string{`{"TimeResolution":"bogus"}`, `{"not":`} {
				_, err := ReportTimeResolution(nil, llotypes.ChannelDefinition{ReportFormat: rf, Opts: []byte(opts)}, 1)
				require.ErrorContains(t, err, "channel opts")
			}
		})
	}

	// Formats with a fixed resolution ignore opts entirely.
	res, err := ReportTimeResolution(nil, llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatEVMPremiumLegacy, Opts: []byte(`{"TimeResolution":"ns"}`)}, 1)
	require.NoError(t, err)
	require.Equal(t, ResolutionSeconds, res)
}
