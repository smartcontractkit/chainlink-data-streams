package llo

import (
	"github.com/smartcontractkit/libocr/offchainreporting2/types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type Report struct {
	ConfigDigest types.ConfigDigest
	// OCR sequence number of this report
	SeqNr uint64
	// Channel that is being reported on
	ChannelID llotypes.ChannelID
	// Report is only valid at t > ValidAfterNanoseconds
	// ValidAfterNanoseconds < ObservationTimestampNanoseconds always, by enforcement
	// in IsReportable
	ValidAfterNanoseconds uint64
	// ObservationTimestampNanoseconds is the median of all observation timestamps
	// (note that this timestamp is taken immediately before we initiate any
	// observations)
	ObservationTimestampNanoseconds uint64
	// Values for every stream in the channel
	Values []StreamValue
	// The contract onchain will only validate non-specimen reports. A staging
	// protocol instance will generate specimen reports so we can validate it
	// works properly without any risk of misreports landing on chain.
	Specimen bool
}
