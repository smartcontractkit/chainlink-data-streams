// This program generates golden files for LLOOutcomeProtoV1 serialization comparison tests.
// It should be run from the v0.1.6 tag to generate the baseline golden files.
package main

import (
	"os"
	"path/filepath"

	"github.com/smartcontractkit/chainlink-data-streams/llo"
)

func main() {
	config := llo.OffchainConfig{
		ProtocolVersion:                     1,
		DefaultMinReportIntervalNanoseconds: 1,
	}
	codec := config.GetOutcomeCodec()

	baseDir := filepath.Join("..", "..")
	if len(os.Args) > 1 {
		baseDir = os.Args[1]
	}

	for _, tc := range llo.GoldenOutcomeCases() {
		serialized, err := codec.Encode(tc.Outcome)
		if err != nil {
			panic(err)
		}

		outputPath := filepath.Join(baseDir, tc.OutputFile)
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			panic(err)
		}

		if err := os.WriteFile(outputPath, serialized, 0644); err != nil {
			panic(err)
		}

		println("Generated:", outputPath)
	}
}
