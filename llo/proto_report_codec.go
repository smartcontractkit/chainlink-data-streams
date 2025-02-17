package llo

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

var DeterministicMarshalOptions = proto.MarshalOptions{Deterministic: true}

var _ ReportEncoder = ProtoReportCodec{}

// ProtoReportCodec is a chain-agnostic reference implementation

type ProtoReportCodec struct{}

func (cdc ProtoReportCodec) Encode(_ context.Context, r Report, cd llotypes.ChannelDefinition) ([]byte, error) {
	p := &LLOReportProto{
		ChannelId:                   r.ChannelID,
		ValidAfterSeconds:           r.ValidAfterSeconds,
		ObservationTimestampSeconds: r.ObservationTimestampSeconds,
		Specimen:                    r.Specimen,
	}
	if len(r.Values) != len(cd.Streams) {
		return nil, fmt.Errorf("values length %d does not match channel definition length %d", len(r.Values), len(cd.Streams))
	}
	p.StreamValues = make(map[uint32]*LLOStreamValue, len(r.Values))
	for i, sv := range r.Values {
		if sv == nil {
			// Nil values will not be encoded into the map
			continue
		}
		b, err := sv.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("failed to encode StreamValue: %w", err)
		}
		strm := cd.Streams[i]
		// TODO: How to handle duplicate stream IDs???
		if _, exists := p.StreamValues[strm.StreamID]; exists {
			return nil, fmt.Errorf("duplicate stream id %d", strm.StreamID)
		}
		p.StreamValues[strm.StreamID] = &LLOStreamValue{
			Type:  sv.Type(),
			Value: b,
		}
	}
	return DeterministicMarshalOptions.Marshal(p)
}

func (cdc ProtoReportCodec) Decode(b []byte) (p *LLOReportProto, err error) {
	p = &LLOReportProto{}
	err = proto.Unmarshal(b, p)
	if err != nil {
		return p, fmt.Errorf("failed to decode report proto: %w", err)
	}
	return p, nil
}

func (cdc ProtoReportCodec) Pack(digest types.ConfigDigest, seqNr uint64, report ocr2types.Report, sigs []types.AttributedOnchainSignature) ([]byte, error) {
	pSigs := make([]*LLOAttributedOnchainSignature, len(sigs))
	for i, sig := range sigs {
		pSigs[i] = &LLOAttributedOnchainSignature{
			Signer:    uint32(sig.Signer),
			Signature: sig.Signature,
		}
	}
	p := &LLOPayloadWithSigsProto{
		ConfigDigest: digest[:],
		SeqNr:        seqNr,
		Report:       report,
		Sigs:         pSigs,
	}
	return proto.Marshal(p)
}

func (cdc ProtoReportCodec) Unpack(b []byte) (p *LLOPayloadWithSigsProto, err error) {
	p = &LLOPayloadWithSigsProto{}
	err = proto.Unmarshal(b, p)
	if err != nil {
		return p, fmt.Errorf("failed to decode report proto: %w", err)
	}
	return p, nil
}

func (cdc ProtoReportCodec) UnpackDecode(b []byte) (p *LLOPayloadWithSigsProto, r *LLOReportProto, err error) {
	p, err = cdc.Unpack(b)
	if err != nil {
		return nil, nil, err
	}
	r, err = cdc.Decode(p.Report)
	if err != nil {
		return nil, nil, err
	}
	return p, r, nil
}
