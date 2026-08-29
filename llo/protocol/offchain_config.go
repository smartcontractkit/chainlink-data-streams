package protocol

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
)

type OffchainConfig struct {
	ProtocolVersion uint32
	// DefaultMinReportIntervalNanoseconds is the default minimum report interval in nanoseconds.
	// It must be set to 0 for protocol version 0.
	// It must be set to 1 or greater for protocol version 1+.
	//
	// NOTE: This merely controls the _minimum_ interval between reports. It
	// does not guarantee a maximum interval. If you want reports to be
	// produced quickly, you are still limited by OCR3's DeltaRound and
	// DeltaGrace params, as well as networking latency.
	DefaultMinReportIntervalNanoseconds uint64
	// DefaultMinObservationIntervalNanoseconds is the default minimum interval
	// in nanoseconds between the last report of a channel and the next time its
	// streams are observed/aggregated. Each channel carries an observation
	// schedule advanced by this interval whenever it reports; until its next
	// slot comes round, its observation and aggregation are skipped entirely.
	//
	// It must be set to 0 for protocol version 0.
	// For protocol version 1+, 0 means disabled (all channels are always
	// observed); a non-zero value enables the skip. It must not exceed
	// DefaultMinReportIntervalNanoseconds, or a channel could be reportable
	// but lack the observations needed to produce a report.
	//
	// Setting it equal to DefaultMinReportIntervalNanoseconds is safe. The first
	// round after a skip window has no stream values yet (they are gathered
	// asynchronously, so they arrive a round later), and a channel with no
	// aggregate withholds its report until they land rather than emitting one
	// full of nils.
	//
	// That delay does not accumulate. The observation schedule advances at a
	// fixed rate from its own previous slot rather than from the round that
	// reported, so a channel configured to report every T keeps reporting every
	// T, offset once by however long its first cycle took to gather.
	//
	// SIZING HISTORY WINDOWS. Enabling this makes a channel's report cadence the
	// sampling rate for any History(...) window it reads, so a window's depth
	// buys a different amount of wall-clock coverage than it did at the round
	// rate. Size depth, and the TWAP window/minSamples/gap thresholds, against
	// this interval.
	//
	// For TWAP specifically, an interval at or below one second changes nothing:
	// TWAP buckets its window by the second and takes the newest record in each
	// bucket, so a faster sampling rate was already being discarded. It makes
	// depth go further, since depth stops being spent on records that collapse
	// into the same bucket. Above one second, the observed bucket count falls to
	// about window/interval and every interior gap becomes interval-1 buckets,
	// which is where minSamples and maxInteriorGap start to bite.
	//
	// The other window functions (Avg, Median, EMA, SMA, WMA, Delta, PctChange,
	// Spread, Variance, Stddev, Last) read the record series directly with no
	// bucketing, so their wall-clock meaning tracks the sampling rate at any
	// interval: EMA(History(s, 50), 20) smooths over a very different span at a
	// 20ms round than at a 1s cadence.
	//
	// Getting this wrong costs reports, not correctness. A window that cannot be
	// satisfied leaves its channel unreportable, which also stops its schedule
	// advancing, so the channel reverts to observing every round until the window
	// is satisfied again.
	DefaultMinObservationIntervalNanoseconds uint64
	// EnableObservationCompression enables observation compression.
	EnableObservationCompression bool
}

func DecodeOffchainConfig(b []byte) (o OffchainConfig, err error) {
	pbuf := &LLOOffchainConfigProto{}
	err = proto.Unmarshal(b, pbuf)
	if err != nil {
		// HACK: We have actual invalid bytes written on-chain, which we have
		// to handle to be compatible with older builds which ignored offchain
		// config.
		//
		// FIXME: Return error instead after v0 is fully decommissioned and all
		// contracts have been updated with proper v1 config.
		//
		// MERC-2272
		return o, nil
		// return o, fmt.Errorf("failed to decode offchain config: expected protobuf (got: 0x%x); %w", b, err)
	}
	if err := o.Validate(); err != nil {
		return o, fmt.Errorf("failed to decode offchain config: %w", err)
	}
	o.ProtocolVersion = pbuf.ProtocolVersion
	o.DefaultMinReportIntervalNanoseconds = pbuf.DefaultMinReportIntervalNanoseconds
	o.DefaultMinObservationIntervalNanoseconds = pbuf.DefaultMinObservationIntervalNanoseconds
	o.EnableObservationCompression = pbuf.EnableObservationCompression
	return
}

func (c OffchainConfig) Encode() ([]byte, error) {
	pbuf := &LLOOffchainConfigProto{
		ProtocolVersion:                          c.ProtocolVersion,
		DefaultMinReportIntervalNanoseconds:      c.DefaultMinReportIntervalNanoseconds,
		DefaultMinObservationIntervalNanoseconds: c.DefaultMinObservationIntervalNanoseconds,
		EnableObservationCompression:             c.EnableObservationCompression,
	}
	return proto.Marshal(pbuf)
}

func (c OffchainConfig) Validate() error {
	switch c.ProtocolVersion {
	case 0:
		if c.DefaultMinReportIntervalNanoseconds != 0 {
			return errors.New("default report cadence must be 0 if protocol version is 0")
		}
		if c.DefaultMinObservationIntervalNanoseconds != 0 {
			return errors.New("default observation cadence must be 0 if protocol version is 0")
		}
	case 1:
		if c.DefaultMinReportIntervalNanoseconds == 0 {
			return errors.New("default report cadence must be non-zero if protocol version is 1")
		}
		if c.DefaultMinObservationIntervalNanoseconds > c.DefaultMinReportIntervalNanoseconds {
			return errors.New("default observation cadence must not exceed default report cadence")
		}
	default:
		return fmt.Errorf("unknown protocol version: %d", c.ProtocolVersion)
	}
	return nil
}
