package llo

import (
	"slices"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// merkleBundler collects the channel reports of a round that are signed as
// leaves, per signing family.
type merkleBundler struct {
	leaves map[llotypes.ReportFormat][]bundlerLeaf
}

type bundlerLeaf struct {
	leaf *protocol.LLOMerkleLeafProto
	// source is the reportable channel the leaf was emitted for. It differs
	// from the leaf channel ID for backfill reports.
	source llotypes.ChannelID
	// emitted captures the report telemetry once the leaf is in a bundle.
	emitted func()
}

// add records the report encoded for channelID in format as a leaf of family.
func (b *merkleBundler) add(family llotypes.ReportFormat, source, channelID llotypes.ChannelID, format llotypes.ReportFormat, report []byte, emitted func()) {
	if b.leaves == nil {
		b.leaves = map[llotypes.ReportFormat][]bundlerLeaf{}
	}
	b.leaves[family] = append(b.leaves[family], bundlerLeaf{
		leaf: &protocol.LLOMerkleLeafProto{
			ChannelID:    channelID,
			ReportFormat: uint32(format),
			Report:       report,
		},
		source:  source,
		emitted: emitted,
	})
}

// bundles returns one bundle report per family, split by size, in family
// order. Leaves are in the order they were added, which is ascending
// reportable channel ID. A family that fails to encode emits none of its
// leaves, and the other families are unaffected.
func (b *merkleBundler) bundles(p *Plugin, seqNr uint64, stage llotypes.LifeCycleStage) []ocr3types.ReportPlus[llotypes.ReportInfo] {
	families := make([]llotypes.ReportFormat, 0, len(b.leaves))
	for family := range b.leaves {
		families = append(families, family)
	}
	slices.Sort(families)

	var rwis []ocr3types.ReportPlus[llotypes.ReportInfo]
	for _, family := range families {
		pending := b.leaves[family]
		leaves := make([]*protocol.LLOMerkleLeafProto, len(pending))
		for i, l := range pending {
			leaves[i] = l.leaf
		}
		encoded, oversized, err := protocol.SplitMerkleBundle(family, leaves, MaxReportBytes)
		if err != nil {
			for _, l := range pending {
				p.reportNotEmitted(seqNr, l.source, llotypes.ReportFormat(l.leaf.ReportFormat), "merkle_bundle_encode_failed", "family", family, "err", err)
			}
			continue
		}
		for _, i := range oversized {
			l := pending[i]
			p.reportNotEmitted(seqNr, l.source, llotypes.ReportFormat(l.leaf.ReportFormat), "merkle_leaf_too_large", "reportBytes", len(l.leaf.Report))
			pending[i].emitted = nil
		}
		for _, l := range pending {
			if l.emitted != nil {
				l.emitted()
			}
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
	return rwis
}
