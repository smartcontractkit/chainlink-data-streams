package llo

import "encoding/json"

type RetirementReportCodec interface {
	Encode(RetirementReport) ([]byte, error)
	Decode([]byte) (RetirementReport, error)
}

var _ RetirementReportCodec = StandardRetirementReportCodec{}

type StandardRetirementReportCodec struct{}

func (r StandardRetirementReportCodec) Encode(report RetirementReport) ([]byte, error) {
	return json.Marshal(report)
}

func (r StandardRetirementReportCodec) Decode(data []byte) (RetirementReport, error) {
	var report RetirementReport
	err := json.Unmarshal(data, &report)
	return report, err
}
