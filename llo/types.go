package llo

import (
	"math/big"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type ChannelDefinitionWithID struct {
	llotypes.ChannelDefinition
	ChannelID llotypes.ChannelID
}

type ChannelHash [32]byte

type OnchainConfigCodec interface {
	Encode(OnchainConfig) ([]byte, error)
	Decode([]byte) (OnchainConfig, error)
}

type Transmitter interface {
	// NOTE: Mercury doesn't actually transmit on-chain, so there is no
	// "contract" involved with the transmitter.
	// - Transmit should be implemented and send to Mercury server
	// - FromAccount() should return CSA public key
	ocr3types.ContractTransmitter[llotypes.ReportInfo]
}

type ObsResult[T any] struct {
	Val   T
	Valid bool
}

type Report struct {
	ConfigDigest types.ConfigDigest
	// Chain the report is destined for
	ChainSelector uint64
	// OCR sequence number of this report
	SeqNr uint64
	// Channel that is being reported on
	ChannelID llotypes.ChannelID
	// Report is valid for ValidAfterSeconds < block.time <= ValidUntilSeconds
	ValidAfterSeconds uint32
	ValidUntilSeconds uint32
	// Here we only encode big.Ints, but in principle there's nothing stopping
	// us from also supporting non-numeric data or smaller values etc...
	Values []*big.Int
	// The contract onchain will only validate non-specimen reports. A staging
	// protocol instance will generate specimen reports so we can validate it
	// works properly without any risk of misreports landing on chain.
	Specimen bool
}
