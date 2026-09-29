package pluginconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	v31 "github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31"
)

// Duration is a time.Duration that decodes from a duration string in both TOML
// and JSON, so the v31 knobs read as "2s" in a job spec.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalJSON accepts a duration string ("2s") or a plain number of
// nanoseconds, matching time.Duration's own JSON encoding.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return d.UnmarshalText([]byte(s))
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("invalid duration: %s", string(data))
	}
	*d = Duration(n)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(time.Duration(d).String())), nil
}

// V31Config carries the v31 plugin knobs. Every field is optional: left at
// zero the plugin applies its own default. Only meaningful when PluginVersion
// is "v31".
type V31Config struct {
	// VerboseLogging enables additional, potentially expensive logging.
	VerboseLogging bool `json:"verboseLogging" toml:"verboseLogging"`

	// MaxSnapshotRounds bounds how stale this node's own stream values may be
	// when it references them. Must leave BlobFetchMarginRounds below
	// BlobLifetimeRounds.
	MaxSnapshotRounds uint64 `json:"maxSnapshotRounds" toml:"maxSnapshotRounds"`

	// BlobLifetimeRounds bounds how long peers can still fetch a broadcast
	// blob. It does not bound staleness, MaxSnapshotRounds does.
	BlobLifetimeRounds uint64 `json:"blobLifetimeRounds" toml:"blobLifetimeRounds"`

	// MaxDurationBlobObservation is the blob pump's per-cycle observation
	// budget.
	MaxDurationBlobObservation Duration `json:"maxDurationBlobObservation" toml:"maxDurationBlobObservation"`

	// BlobInFlightWaitFactor divides MaxDurationObservation into how long Take
	// waits for a cycle already in flight to park, so a larger factor waits
	// less.
	BlobInFlightWaitFactor uint64 `json:"blobInFlightWaitFactor" toml:"blobInFlightWaitFactor"`

	// MaxBlobSnapshotAge pins the wall-clock age at which a parked snapshot is
	// discarded. Left at zero the pump derives it from the round period it
	// measures. A negative value disables the check, leaving MaxSnapshotRounds
	// as the only staleness bound.
	MaxBlobSnapshotAge Duration `json:"maxBlobSnapshotAge" toml:"maxBlobSnapshotAge"`

	// MaxRoundPeriod bounds the round period the pump measures, so a stalled
	// round cannot inflate the derived snapshot age bound. Must be set above the
	// DON real round cadence.
	MaxRoundPeriod Duration `json:"maxRoundPeriod" toml:"maxRoundPeriod"`
}

// IsZero reports whether no knob is set, so the caller can tell an absent block
// from one that configures defaults.
func (o V31Config) IsZero() bool {
	return o == V31Config{}
}

func (o V31Config) Validate() (merr error) {
	if _, _, err := v31.ResolveBlobRounds(o.MaxSnapshotRounds, o.BlobLifetimeRounds); err != nil {
		merr = errors.Join(merr, fmt.Errorf("llo: v31: %w", err))
	}
	if o.MaxDurationBlobObservation < 0 {
		merr = errors.Join(merr, fmt.Errorf("llo: v31: MaxDurationBlobObservation must not be negative, got: %s", o.MaxDurationBlobObservation))
	}
	if o.MaxRoundPeriod < 0 {
		merr = errors.Join(merr, fmt.Errorf("llo: v31: MaxRoundPeriod must not be negative, got: %s", o.MaxRoundPeriod))
	}
	return merr
}
