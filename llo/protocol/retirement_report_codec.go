package protocol

import (
	"fmt"

	"github.com/goccy/go-json"
)

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
	if err := json.Unmarshal(data, &report); err != nil {
		return RetirementReport{}, err
	}
	if n := len(report.ValidAfterNanoseconds); n > MaxRetirementReportChannels {
		return RetirementReport{}, fmt.Errorf("retirement report has %d channels, max %d", n, MaxRetirementReportChannels)
	}
	return report, nil
}
