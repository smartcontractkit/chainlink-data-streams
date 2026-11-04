package llo

import (
	"fmt"
	"slices"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// merkleBundleMaxBytes is the MaxReportBytes NewReportingPlugin declares. A
// bundle is one report, so it must fit in it.
const merkleBundleMaxBytes = ocr3_1types.MaxMaxReportBytes

// merkleBundler collects the channel reports of a round that are signed as
// leaves, per signing family.
type merkleBundler struct {
	leaves map[llotypes.ReportFormat][]*protocol.LLOMerkleLeafProto
	// sources are the reportable channels the leaves were emitted for, in leaf
	// order. It differs from the leaf channel ID for backfill reports.
	sources map[llotypes.ReportFormat][]llotypes.ChannelID
}

// add records the report encoded for channelID in format as a leaf of family.
func (b *merkleBundler) add(family llotypes.ReportFormat, source, channelID llotypes.ChannelID, format llotypes.ReportFormat, report []byte) {
	if b.leaves == nil {
		b.leaves = map[llotypes.ReportFormat][]*protocol.LLOMerkleLeafProto{}
		b.sources = map[llotypes.ReportFormat][]llotypes.ChannelID{}
	}
	b.leaves[family] = append(b.leaves[family], &protocol.LLOMerkleLeafProto{
		ChannelID:    channelID,
		ReportFormat: uint32(format),
		Report:       report,
	})
	b.sources[family] = append(b.sources[family], source)
}

// signingFamily decides how the report of a channel encoded with cd is signed.
// It returns the family when the report is a leaf, and false when it is signed
// on its own. A scheme the instance cannot sign with returns an error and the
// report is not emitted.
//
// Baseline verification keeps such a scheme out of the committed set. The
// error path only guards against it getting there anyway.
func (p *Plugin) signingFamily(cd llotypes.ChannelDefinition) (llotypes.ReportFormat, bool, error) {
	switch cd.SigningScheme {
	case llotypes.SigningSchemeDefault:
		return 0, false, nil
	case llotypes.SigningSchemeMerkle:
		family, ok := protocol.MerkleFamily(cd.ReportFormat)
		if !ok {
			return 0, false, fmt.Errorf("report format %s has no signing family", cd.ReportFormat)
		}
		return family, true, nil
	default:
		return 0, false, fmt.Errorf("unknown signing scheme %q", cd.SigningScheme)
	}
}

// bundles returns one bundle report per family, split by size, in family
// order. Leaves are in the order they were added, which is ascending
// reportable channel ID.
func (b *merkleBundler) bundles(p *Plugin, seqNr uint64, stage llotypes.LifeCycleStage) ([]ocr3types.ReportPlus[llotypes.ReportInfo], error) {
	families := make([]llotypes.ReportFormat, 0, len(b.leaves))
	for family := range b.leaves {
		families = append(families, family)
	}
	slices.Sort(families)

	var rwis []ocr3types.ReportPlus[llotypes.ReportInfo]
	for _, family := range families {
		encoded, oversized, err := protocol.SplitMerkleBundle(family, b.leaves[family], merkleBundleMaxBytes)
		if err != nil {
			return nil, fmt.Errorf("error encoding %s bundle: %w", family, err)
		}
		for _, i := range oversized {
			leaf := b.leaves[family][i]
			p.reportNotEmitted(seqNr, b.sources[family][i], llotypes.ReportFormat(leaf.ReportFormat), "merkle_leaf_too_large", "reportBytes", len(leaf.Report))
		}
		for _, bundle := range encoded {
			rwis = append(rwis, ocr3types.ReportPlus[llotypes.ReportInfo]{
				ReportWithInfo: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
					Report: bundle,
					Info: llotypes.ReportInfo{
						LifeCycleStage: stage,
						ReportFormat:   family,
					},
				},
			})
		}
	}
	return rwis, nil
}
