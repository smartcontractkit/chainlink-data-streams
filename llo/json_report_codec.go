package llo

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

var _ ReportCodec = JSONReportCodec{}

// JSONReportCodec is a chain-agnostic reference implementation

type JSONReportCodec struct{}

func (cdc JSONReportCodec) Encode(r Report, _ llotypes.ChannelDefinition) ([]byte, error) {
	type encode struct {
		ConfigDigest                    types.ConfigDigest
		SeqNr                           uint64
		ChannelID                       llotypes.ChannelID
		ValidAfterNanoseconds           uint64
		ObservationTimestampNanoseconds uint64
		Values                          []TypedTextStreamValue
		Specimen                        bool
	}
	values := make([]TypedTextStreamValue, len(r.Values))
	for i, sv := range r.Values {
		t, err := NewTypedTextStreamValue(sv)
		if err != nil {
			return nil, fmt.Errorf("failed to encode StreamValue: %w", err)
		}
		values[i] = t
	}
	e := encode{
		ConfigDigest:                    r.ConfigDigest,
		SeqNr:                           r.SeqNr,
		ChannelID:                       r.ChannelID,
		ValidAfterNanoseconds:           r.ValidAfterNanoseconds,
		ObservationTimestampNanoseconds: r.ObservationTimestampNanoseconds,
		Values:                          values,
		Specimen:                        r.Specimen,
	}
	return json.Marshal(e)
}

func (cdc JSONReportCodec) Verify(cd llotypes.ChannelDefinition) error {
	if len(cd.Opts) > 0 {
		return fmt.Errorf("unexpected Opts in ChannelDefinition (JSONReportCodec expects no opts), got: %q", cd.Opts)
	}
	return nil
}

func (cdc JSONReportCodec) Decode(b []byte) (r Report, err error) {
	type decode struct {
		ConfigDigest                    string
		SeqNr                           uint64
		ChannelID                       llotypes.ChannelID
		ValidAfterNanoseconds           uint64
		ObservationTimestampNanoseconds uint64
		Values                          []TypedTextStreamValue
		Specimen                        bool
	}
	d := decode{}
	err = json.Unmarshal(b, &d)
	if err != nil {
		return r, fmt.Errorf("failed to decode report: expected JSON (got: %s); %w", b, err)
	}
	if d.SeqNr == 0 {
		// catch obviously bad inputs, since a valid report can never have SeqNr == 0
		return r, errors.New("missing SeqNr")
	}

	cdBytes, err := hex.DecodeString(d.ConfigDigest)
	if err != nil {
		return r, fmt.Errorf("invalid ConfigDigest; %w", err)
	}
	cd, err := types.BytesToConfigDigest(cdBytes)
	if err != nil {
		return r, fmt.Errorf("invalid ConfigDigest; %w", err)
	}
	values := make([]StreamValue, len(d.Values))
	for i := range d.Values {
		values[i], err = UnmarshalTypedTextStreamValue(&d.Values[i])
		if err != nil {
			return r, fmt.Errorf("failed to decode StreamValue: %w", err)
		}
	}

	return Report{
		ConfigDigest:                    cd,
		SeqNr:                           d.SeqNr,
		ChannelID:                       d.ChannelID,
		ValidAfterNanoseconds:           d.ValidAfterNanoseconds,
		ObservationTimestampNanoseconds: d.ObservationTimestampNanoseconds,
		Values:                          values,
		Specimen:                        d.Specimen,
	}, err
}

func (cdc JSONReportCodec) Pack(digest types.ConfigDigest, seqNr uint64, report ocr2types.Report, sigs []types.AttributedOnchainSignature) ([]byte, error) {
	type packed struct {
		ConfigDigest types.ConfigDigest                 `json:"configDigest"`
		SeqNr        uint64                             `json:"seqNr"`
		Report       json.RawMessage                    `json:"report"`
		Sigs         []types.AttributedOnchainSignature `json:"sigs"`
	}
	p := packed{
		ConfigDigest: digest,
		SeqNr:        seqNr,
		Report:       json.RawMessage(report),
		Sigs:         sigs,
	}
	return json.Marshal(p)
}

func (cdc JSONReportCodec) Unpack(b []byte) (digest types.ConfigDigest, seqNr uint64, report ocr2types.Report, sigs []types.AttributedOnchainSignature, err error) {
	type packed struct {
		ConfigDigest string                             `json:"configDigest"`
		SeqNr        uint64                             `json:"seqNr"`
		Report       json.RawMessage                    `json:"report"`
		Sigs         []types.AttributedOnchainSignature `json:"sigs"`
	}
	p := packed{}
	err = json.Unmarshal(b, &p)
	if err != nil {
		return digest, seqNr, report, sigs, fmt.Errorf("failed to unpack report: expected JSON (got: %s); %w", b, err)
	}
	cdBytes, err := hex.DecodeString(p.ConfigDigest)
	if err != nil {
		return digest, seqNr, report, sigs, fmt.Errorf("invalid ConfigDigest; %w", err)
	}
	cd, err := types.BytesToConfigDigest(cdBytes)
	if err != nil {
		return digest, seqNr, report, sigs, fmt.Errorf("invalid ConfigDigest; %w", err)
	}
	return cd, p.SeqNr, ocr2types.Report(p.Report), p.Sigs, nil
}

func (cdc JSONReportCodec) UnpackDecode(b []byte) (digest types.ConfigDigest, seqNr uint64, report Report, sigs []types.AttributedOnchainSignature, err error) {
	var encodedReport []byte
	digest, seqNr, encodedReport, sigs, err = cdc.Unpack(b)
	if err != nil {
		return digest, seqNr, report, sigs, err
	}
	r, err := cdc.Decode(encodedReport)
	if err != nil {
		return digest, seqNr, report, sigs, err
	}
	return digest, seqNr, r, sigs, nil
}
