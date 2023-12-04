package llo

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// We assume here that streams are identified by some human readable string
// like "eth-usd". Any other identifier type would also do, e.g. uint32,
// [32]byte or whatever else we may prefer.
type StreamID string

func (s StreamID) String() string {
	return string(s)
}

// We assume here that channels are identified by a 32 byte value. Any other
// identifier type would also do. The type choice should primarily be driven
// by smart contract design considerations.
//
// TODO: Add more docs - does it need to begin with a solidity function identifier?
// For example, if we used the default method name, "decode" and the schema was: (bytes32,uint256,int256,int256,int256) then we could make the first 4 bytes "0x276a9bfd" and easily do a reverse lookup to find the schema.
// https://www.4byte.directory/signatures/?bytes4_signature=0x276a9bfd
// If your feed has the schema (bytes32,uint256,int256,int256,int256)  then you can check the signature for decode(bytes32,uint256,int256,int256,int256) and see that it is 0x276a9bfd. Those bytes would be the first four bytes of the feed/stream ID. You could also use it in reverse, and look up the first four bytes in dictionary of function selectors. (We would have to publish all variations we know about somewhere, but there are services and open source directories for this kind of thing.)
// Similar to (but not identical to) the old concept of "feed ID"
type ChannelID [32]byte

func (c ChannelID) Less(other ChannelID) bool {
	return bytes.Compare(c[:], other[:]) < 0
}

func (c ChannelID) MarshalText() (text []byte, err error) {
	return []byte(hex.EncodeToString(c[:])), nil
}

func (c *ChannelID) UnmarshalText(text []byte) (err error) {
	if len(text) != len(c)*2 {
		return fmt.Errorf("invalid channel id length")
	}
	_, err = hex.Decode(c[:], text)
	return err
}

type OnchainConfigCodec interface {
	Encode(OnchainConfig) ([]byte, error)
	Decode([]byte) (OnchainConfig, error)
}

type Transmitter interface {
	// NOTE: Mercury doesn't actually transmit on-chain, so there is no
	// "contract" involved with the transmitter.
	// - Transmit should be implemented and send to Mercury server
	// - FromAccount() should return CSA public key
	ocr3types.ContractTransmitter[ReportInfo]
}

type ObsResult[T any] struct {
	Val T
	Err error
}
