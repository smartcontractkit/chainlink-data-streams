package llo

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

var _ ReportCodec = JSONReportCodec{}

type JSONStreamValue struct {
	Type  LLOStreamValue_Type
	Value string
}

func UnmarshalJSONStreamValue(enc *JSONStreamValue) (StreamValue, error) {
	if enc == nil {
		return nil, ErrNilStreamValue
	}
	switch enc.Type {
	case LLOStreamValue_Decimal:
		sv := new(Decimal)
		if err := (sv).UnmarshalText([]byte(enc.Value)); err != nil {
			return nil, err
		}
		return sv, nil
	case LLOStreamValue_Quote:
		sv := new(Quote)
		if err := (sv).UnmarshalText([]byte(enc.Value)); err != nil {
			return nil, err
		}
		return sv, nil
	default:
		return nil, fmt.Errorf("unknown StreamValueType %d", enc.Type)
	}
}

// JSONReportCodec is a chain-agnostic reference implementation

type JSONReportCodec struct{}

func (cdc JSONReportCodec) Encode(ctx context.Context, r Report, _ llotypes.ChannelDefinition) ([]byte, error) {
	type encode struct {
		ConfigDigest                types.ConfigDigest
		SeqNr                       uint64
		ChannelID                   llotypes.ChannelID
		ValidAfterSeconds           uint32
		ObservationTimestampSeconds uint32
		Values                      []JSONStreamValue
		Specimen                    bool
	}
	values := make([]JSONStreamValue, len(r.Values))
	for i, sv := range r.Values {
		if sv == nil {
			return nil, ErrNilStreamValue
		}
		b, err := sv.MarshalText()
		if err != nil {
			return nil, fmt.Errorf("failed to encode StreamValue: %w", err)
		}
		values[i] = JSONStreamValue{
			Type:  sv.Type(),
			Value: string(b),
		}
	}
	e := encode{
		ConfigDigest:                r.ConfigDigest,
		SeqNr:                       r.SeqNr,
		ChannelID:                   r.ChannelID,
		ValidAfterSeconds:           r.ValidAfterSeconds,
		ObservationTimestampSeconds: r.ObservationTimestampSeconds,
		Values:                      values,
		Specimen:                    r.Specimen,
	}
	return json.Marshal(e)
}

// Fuzz testing: MERC-6522
func (cdc JSONReportCodec) Decode(b []byte) (r Report, err error) {
	type decode struct {
		ConfigDigest                string
		SeqNr                       uint64
		ChannelID                   llotypes.ChannelID
		ValidAfterSeconds           uint32
		ObservationTimestampSeconds uint32
		Values                      []JSONStreamValue
		Specimen                    bool
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
	values := make([]StreamValue, len(d.Values))
	for i := range d.Values {
		values[i], err = UnmarshalJSONStreamValue(&d.Values[i])
		if err != nil {
			return r, fmt.Errorf("failed to decode StreamValue: %w", err)
		}
	}
	if d.SeqNr == 0 {
		// catch obviously bad inputs, since a valid report can never have SeqNr == 0
		return r, fmt.Errorf("missing SeqNr")
	}

	return Report{
		ConfigDigest:                cd,
		SeqNr:                       d.SeqNr,
		ChannelID:                   d.ChannelID,
		ValidAfterSeconds:           d.ValidAfterSeconds,
		ObservationTimestampSeconds: d.ObservationTimestampSeconds,
		Values:                      values,
		Specimen:                    d.Specimen,
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
