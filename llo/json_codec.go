package llo

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	commontypes "github.com/smartcontractkit/chainlink-common/pkg/types"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
)

var _ ReportCodec = JSONReportCodec{}

// JSONReportCodec is a chain-agnostic reference implementation

type JSONReportCodec struct{}

func (cdc JSONReportCodec) Encode(r Report) ([]byte, error) {
	return json.Marshal(r)
}

func (cdc JSONReportCodec) Decode(b []byte) (r Report, err error) {
	type decode struct {
		ConfigDigest      string
		ChainSelector     uint64
		SeqNr             uint64
		ChannelID         commontypes.ChannelID
		ValidAfterSeconds uint32
		ValidUntilSeconds uint32
		Values            []*big.Int
		Specimen          bool
	}
	d := decode{}
	err = json.Unmarshal(b, &d)
	if err != nil {
		return r, fmt.Errorf("failed to decode report: expected JSON (got: %s); %w", b, err)
	}
	cdBytes, err := hex.DecodeString(d.ConfigDigest)
	if err != nil {
		return r, fmt.Errorf("invalid ConfigDigest; %w", err)
	}
	cd, err := types.BytesToConfigDigest(cdBytes)
	if err != nil {
		return r, fmt.Errorf("invalid ConfigDigest; %w", err)
	}

	return Report{
		ConfigDigest:      cd,
		ChainSelector:     d.ChainSelector,
		SeqNr:             d.SeqNr,
		ChannelID:         d.ChannelID,
		ValidAfterSeconds: d.ValidAfterSeconds,
		ValidUntilSeconds: d.ValidUntilSeconds,
		Values:            d.Values,
		Specimen:          d.Specimen,
	}, err
}
