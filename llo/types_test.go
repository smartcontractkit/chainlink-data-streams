package llo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDuration_String(t *testing.T) {
	tests := []struct {
		name     string
		d        Duration
		expected string
	}{
		{"zero", Duration(0), "0s"},
		{"one second", Duration(time.Second), "1s"},
		{"five minutes", Duration(5 * time.Minute), "5m0s"},
		{"complex", Duration(2*time.Hour + 30*time.Minute + 15*time.Second), "2h30m15s"},
		{"sub-second", Duration(500 * time.Millisecond), "500ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.d.String())
		})
	}
}

func TestDuration_MarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		d        Duration
		expected string
	}{
		{"zero", Duration(0), `"0s"`},
		{"one second", Duration(time.Second), `"1s"`},
		{"five minutes", Duration(5 * time.Minute), `"5m0s"`},
		{"negative", Duration(-3 * time.Second), `"-3s"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := tc.d.MarshalJSON()
			require.NoError(t, err)
			assert.Equal(t, tc.expected, string(b))
		})
	}
}

func TestDuration_UnmarshalJSON(t *testing.T) {
	t.Run("string values", func(t *testing.T) {
		tests := []struct {
			name     string
			input    string
			expected Duration
		}{
			{"seconds", `"5s"`, Duration(5 * time.Second)},
			{"minutes", `"10m"`, Duration(10 * time.Minute)},
			{"complex", `"1h30m"`, Duration(time.Hour + 30*time.Minute)},
			{"milliseconds", `"250ms"`, Duration(250 * time.Millisecond)},
			{"negative", `"-2s"`, Duration(-2 * time.Second)},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var d Duration
				err := d.UnmarshalJSON([]byte(tc.input))
				require.NoError(t, err)
				assert.Equal(t, tc.expected, d)
			})
		}
	})

	t.Run("numeric values (nanoseconds)", func(t *testing.T) {
		tests := []struct {
			name     string
			input    string
			expected Duration
		}{
			{"zero", `0`, Duration(0)},
			{"one billion (1s)", `1000000000`, Duration(time.Second)},
			{"fractional", `1500000000.0`, Duration(1500 * time.Millisecond)},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var d Duration
				err := d.UnmarshalJSON([]byte(tc.input))
				require.NoError(t, err)
				assert.Equal(t, tc.expected, d)
			})
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		tests := []struct {
			name  string
			input string
		}{
			{"invalid JSON", `{`},
			{"invalid duration string", `"notaduration"`},
			{"boolean", `true`},
			{"null", `null`},
			{"array", `[1,2]`},
			{"object", `{"key":"val"}`},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var d Duration
				err := d.UnmarshalJSON([]byte(tc.input))
				assert.Error(t, err)
			})
		}
	})

	t.Run("roundtrip", func(t *testing.T) {
		original := Duration(5*time.Minute + 30*time.Second)
		b, err := original.MarshalJSON()
		require.NoError(t, err)

		var decoded Duration
		err = decoded.UnmarshalJSON(b)
		require.NoError(t, err)
		assert.Equal(t, original, decoded)
	})
}

func TestTimeResolution_MarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		tp       TimeResolution
		expected string
	}{
		{"seconds", ResolutionSeconds, `"s"`},
		{"milliseconds", ResolutionMilliseconds, `"ms"`},
		{"microseconds", ResolutionMicroseconds, `"us"`},
		{"nanoseconds", ResolutionNanoseconds, `"ns"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := tc.tp.MarshalJSON()
			require.NoError(t, err)
			assert.Equal(t, tc.expected, string(b))
		})
	}

	t.Run("invalid resolution returns error", func(t *testing.T) {
		tp := TimeResolution(255)
		_, err := tp.MarshalJSON()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid timestamp resolution")
	})
}

func TestTimeResolution_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected TimeResolution
	}{
		{"seconds", `"s"`, ResolutionSeconds},
		{"milliseconds", `"ms"`, ResolutionMilliseconds},
		{"microseconds", `"us"`, ResolutionMicroseconds},
		{"nanoseconds", `"ns"`, ResolutionNanoseconds},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var tp TimeResolution
			err := tp.UnmarshalJSON([]byte(tc.input))
			require.NoError(t, err)
			assert.Equal(t, tc.expected, tp)
		})
	}

	t.Run("invalid string", func(t *testing.T) {
		var tp TimeResolution
		err := tp.UnmarshalJSON([]byte(`"hours"`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid timestamp resolution")
	})

	t.Run("invalid JSON", func(t *testing.T) {
		var tp TimeResolution
		err := tp.UnmarshalJSON([]byte(`123`))
		require.Error(t, err)
	})

	t.Run("roundtrip", func(t *testing.T) {
		for _, res := range []TimeResolution{ResolutionSeconds, ResolutionMilliseconds, ResolutionMicroseconds, ResolutionNanoseconds} {
			b, err := res.MarshalJSON()
			require.NoError(t, err)

			var decoded TimeResolution
			err = decoded.UnmarshalJSON(b)
			require.NoError(t, err)
			assert.Equal(t, res, decoded)
		}
	})
}

func TestConvertTimestamp(t *testing.T) {
	const tsNanos uint64 = 1_700_000_000_000_000_000 // 1.7e18 ns

	tests := []struct {
		name       string
		nanos      uint64
		resolution TimeResolution
		expected   uint64
	}{
		{"to seconds", tsNanos, ResolutionSeconds, tsNanos / 1e9},
		{"to milliseconds", tsNanos, ResolutionMilliseconds, tsNanos / 1e6},
		{"to microseconds", tsNanos, ResolutionMicroseconds, tsNanos / 1e3},
		{"to nanoseconds", tsNanos, ResolutionNanoseconds, tsNanos},
		{"zero value", 0, ResolutionSeconds, 0},
		{"unknown resolution falls back to nanos", tsNanos, TimeResolution(255), tsNanos},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, ConvertTimestamp(tc.nanos, tc.resolution))
		})
	}
}

func TestScaleSeconds(t *testing.T) {
	const secs uint32 = 3600 // 1 hour

	tests := []struct {
		name       string
		seconds    uint32
		resolution TimeResolution
		expected   uint64
	}{
		{"to seconds", secs, ResolutionSeconds, 3600},
		{"to milliseconds", secs, ResolutionMilliseconds, 3_600_000},
		{"to microseconds", secs, ResolutionMicroseconds, 3_600_000_000},
		{"to nanoseconds", secs, ResolutionNanoseconds, 3_600_000_000_000},
		{"zero value", 0, ResolutionNanoseconds, 0},
		{"unknown resolution falls back to seconds", secs, TimeResolution(255), 3600},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, ScaleSeconds(tc.seconds, tc.resolution))
		})
	}
}
