package llo

import (
	"fmt"
	"slices"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/commontypes"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// Telemetry is emitted best-effort on buffered channels. A full channel drops
// the datum rather than blocking the protocol. Emission is an unobservable side
// effect and does not affect StateTransition/Reports return values or state.

// captureOutcomeTelemetry emits the outcome of rounds whose stage emits
// telemetry, when this oracle is one of the f+1 telemetry emitters for seqNr.
// Every oracle computes the same outcome, so f+1 copies suffice.
func (p *Plugin) captureOutcomeTelemetry(out precursor, seqNr uint64) {
	if p.OutcomeTelemetryCh == nil || !p.emitsTelemetry(out.LifeCycleStage) || !p.isTelemetryEmitter(seqNr) {
		return
	}
	ot, err := makeOutcomeTelemetry(out, p.ConfigDigest, seqNr, p.DonID, p.minContributions())
	if err != nil {
		p.Logger.Warnw("Error making outcome telemetry", "err", err)
		return
	}
	select {
	case p.OutcomeTelemetryCh <- ot:
	default:
		p.Logger.Warn("OutcomeTelemetryCh is full, dropping telemetry")
	}
}

func makeOutcomeTelemetry(out precursor, configDigest ocrtypes.ConfigDigest, seqNr uint64, donID uint32, minContributions int) (*protocol.LLOOutcomeTelemetry, error) {
	ot := &protocol.LLOOutcomeTelemetry{
		LifeCycleStage:                  string(out.LifeCycleStage),
		ObservationTimestampNanoseconds: out.ObservationTimestampNanoseconds,
		ChannelDefinitions:              make(map[uint32]*protocol.LLOChannelDefinitionProto, len(out.ChannelDefinitions)),
		ValidAfterNanoseconds:           make(map[uint32]uint64, len(out.ValidAfterNanoseconds)),
		StreamAggregates:                make(map[uint32]*protocol.LLOAggregatorStreamValue, len(out.StreamAggregates)),
		SeqNr:                           seqNr,
		ConfigDigest:                    configDigest[:],
		DonId:                           donID,
		MinContributions:                uint32(minContributions),
	}
	for id, cd := range out.ChannelDefinitions {
		ot.ChannelDefinitions[id] = protocol.ChannelDefinitionToProto(cd)
	}
	for id, va := range out.ValidAfterNanoseconds {
		ot.ValidAfterNanoseconds[id] = va
	}
	for sid, aggMap := range out.StreamAggregates {
		if len(aggMap) == 0 {
			continue
		}
		aggVals := make(map[uint32]*protocol.LLOStreamValue, len(aggMap))
		for agg, sv := range aggMap {
			v, err := protocol.StreamValueToProto(sv)
			if err != nil {
				return nil, fmt.Errorf("failed to make outcome telemetry; %w", err)
			}
			aggVals[uint32(agg)] = v
		}
		ot.StreamAggregates[sid] = &protocol.LLOAggregatorStreamValue{AggregatorValues: aggVals}
	}
	return ot, nil
}

// captureReportTelemetry emits the telemetry of reports produced in a stage
// that emits telemetry, when this oracle is one telemetry emitter for the seqNr.
func (p *Plugin) captureReportTelemetry(r protocol.Report, cd llotypes.ChannelDefinition, stage llotypes.LifeCycleStage) {
	if p.ReportTelemetryCh == nil || !p.emitsTelemetry(stage) || !p.isTelemetryEmitter(r.SeqNr) {
		return
	}
	rt, err := makeReportTelemetry(r, cd, p.DonID)
	if err != nil {
		p.Logger.Warnw("Error making report telemetry", "err", err)
		return
	}
	select {
	case p.ReportTelemetryCh <- rt:
	default:
		p.Logger.Warn("ReportTelemetryCh is full, dropping telemetry")
	}
}

func makeReportTelemetry(r protocol.Report, cd llotypes.ChannelDefinition, donID uint32) (*protocol.LLOReportTelemetry, error) {
	streams := make([]*protocol.LLOStreamDefinition, len(cd.Streams))
	for i, s := range cd.Streams {
		streams[i] = &protocol.LLOStreamDefinition{
			StreamID:   s.StreamID,
			Aggregator: uint32(s.Aggregator),
		}
	}
	svs := make([]*protocol.LLOStreamValue, len(r.Values))
	for i, v := range r.Values {
		if v == nil {
			// Missing stream value (allowed when DisableNilStreamValues is false);
			// emit an empty entry rather than panicking.
			svs[i] = &protocol.LLOStreamValue{}
			continue
		}
		b, err := v.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("error marshalling stream value: %w", err)
		}
		svs[i] = &protocol.LLOStreamValue{
			Type:  v.Type(),
			Value: b,
		}
	}
	rt := &protocol.LLOReportTelemetry{
		ChannelId:                       r.ChannelID,
		ValidAfterNanoseconds:           r.ValidAfterNanoseconds,
		ObservationTimestampNanoseconds: r.ObservationTimestampNanoseconds,
		ReportFormat:                    uint32(cd.ReportFormat),
		Specimen:                        r.Specimen,
		StreamDefinitions:               streams,
		StreamValues:                    svs,
		ChannelOpts:                     cd.Opts,
		SeqNr:                           r.SeqNr,
		ConfigDigest:                    r.ConfigDigest[:],
		DonId:                           donID,
	}
	return rt, nil
}

// maxAttributedObservationTelemetryBytes bounds the serialized size of one
// attributed observation telemetry message. A decoded observation may reach
// protocol.MaxDecompressedObservationLength, so larger ones are split.
const maxAttributedObservationTelemetryBytes = 1 << 20

// attributedObservationTelemetryQueueSize bounds the rounds waiting to be built.
// A full queue drops the round rather than blocking StateTransition.
const attributedObservationTelemetryQueueSize = 8

// attributedObservation is one entry of the round attributed observations, as
// decoded by StateTransition. decodeErr is set when the observation was dropped
// as invalid.
type attributedObservation struct {
	observer  commontypes.OracleID
	obs       Observation
	decodeErr error
}

type attributedObservationRound struct {
	seqNr                           uint64
	agreedObservationTimestampNanos uint64
	aos                             []attributedObservation
}

// isTelemetryEmitter reports whether this oracle is one of the f+1 oracles
// emitting plugin telemetry for this seqNr.
func (p *Plugin) isTelemetryEmitter(seqNr uint64) bool {
	if p.N <= 0 {
		return false
	}
	n := uint64(p.N)
	// (seqNr+i) % N == OracleID for the smallest i in [0, N).
	i := (uint64(p.OracleID) + n - seqNr%n) % n
	return i <= uint64(p.F)
}

// collectAttributedObservations reports whether StateTransition should keep the
// decoded observations of seqNr for telemetry.
func (p *Plugin) collectAttributedObservations(seqNr uint64) bool {
	return p.attributedObservationTelemeter != nil && p.isTelemetryEmitter(seqNr)
}

// emitsTelemetry reports whether a round in stage emits plugin telemetry.
// Production always does. Staging only with Config.CaptureStagingTelemetry, so a
// staging instance can be verified from telemetry before it is promoted. Any
// other stage emits nothing.
func (p *Plugin) emitsTelemetry(stage llotypes.LifeCycleStage) bool {
	switch stage {
	case protocol.LifeCycleStageProduction:
		return true
	case protocol.LifeCycleStageStaging:
		return p.Config.CaptureStagingTelemetry
	default:
		return false
	}
}

// captureAttributedObservationTelemetry hands the round decoded observations
// to the telemeter.
func (p *Plugin) captureAttributedObservationTelemetry(out precursor, seqNr uint64, aos []attributedObservation) {
	if len(aos) == 0 || !p.emitsTelemetry(out.LifeCycleStage) {
		return
	}
	p.attributedObservationTelemeter.enqueue(attributedObservationRound{
		seqNr:                           seqNr,
		agreedObservationTimestampNanos: out.ObservationTimestampNanoseconds,
		aos:                             aos,
	})
}

// attributedObservationTelemeter builds attributed observation telemetry off the
// StateTransition path. It only reads the decoded observations, which are shared
// with the round tally and the blob payload memo.
type attributedObservationTelemeter struct {
	lggr         logger.Logger
	configDigest ocrtypes.ConfigDigest
	donID        uint32
	emitter      commontypes.OracleID
	maxBytes     int

	in        chan attributedObservationRound
	out       chan<- *protocol.LLOAttributedObservationTelemetry
	stop      chan struct{}
	done      chan struct{}
	started   bool
	closeOnce sync.Once
}

func newAttributedObservationTelemeter(lggr logger.Logger, out chan<- *protocol.LLOAttributedObservationTelemetry, configDigest ocrtypes.ConfigDigest, donID uint32, emitter commontypes.OracleID) *attributedObservationTelemeter {
	t := &attributedObservationTelemeter{
		lggr:         lggr,
		configDigest: configDigest,
		donID:        donID,
		emitter:      emitter,
		maxBytes:     maxAttributedObservationTelemetryBytes,
		in:           make(chan attributedObservationRound, attributedObservationTelemetryQueueSize),
		out:          out,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	return t
}

// start runs the telemeter until Close. Call it at most once, before Close.
func (t *attributedObservationTelemeter) start() {
	t.started = true
	go t.run()
}

func (t *attributedObservationTelemeter) enqueue(r attributedObservationRound) {
	select {
	case t.in <- r:
	default:
		t.lggr.Warnw("Attributed observation telemetry queue is full, dropping round", "seqNr", r.seqNr)
	}
}

// Close stops the telemeter and waits for the round in progress to finish.
// Queued rounds are dropped.
func (t *attributedObservationTelemeter) Close() {
	t.closeOnce.Do(func() { close(t.stop) })
	if t.started {
		<-t.done
	}
}

func (t *attributedObservationTelemeter) run() {
	defer close(t.done)
	for {
		select {
		case <-t.stop:
			return
		case r := <-t.in:
			for _, ao := range r.aos {
				for _, msg := range t.makeTelemetry(r.seqNr, r.agreedObservationTimestampNanos, ao) {
					select {
					case t.out <- msg:
					default:
						t.lggr.Warnw("AttributedObservationTelemetryCh is full, dropping telemetry", "seqNr", r.seqNr, "observer", ao.observer)
					}
				}
			}
		}
	}
}

// makeTelemetry builds the telemetry of one observation, split in parts of at
// most maxBytes. Stream values are packed in ascending stream id order. The
// header is repeated in every part, the votes and decode error are set in the
// first only. A single value larger than maxBytes gets a part of its own.
func (t *attributedObservationTelemeter) makeTelemetry(seqNr, agreedTs uint64, ao attributedObservation) []*protocol.LLOAttributedObservationTelemetry {
	header := func() *protocol.LLOAttributedObservationTelemetry {
		return &protocol.LLOAttributedObservationTelemetry{
			ConfigDigest:                          t.configDigest[:],
			SeqNr:                                 seqNr,
			DonId:                                 t.donID,
			Observer:                              uint32(ao.observer),
			Emitter:                               uint32(t.emitter),
			OracleObservationTimestampNanoseconds: ao.obs.UnixTimestampNanoseconds,
			AgreedObservationTimestampNanoseconds: agreedTs,
		}
	}

	first := header()
	if ao.decodeErr != nil {
		first.DecodeError = proto.String(ao.decodeErr.Error())
		return []*protocol.LLOAttributedObservationTelemetry{first}
	}
	if len(ao.obs.RemoveChannelIDs) > 0 {
		first.RemoveChannelIds = make([]uint32, 0, len(ao.obs.RemoveChannelIDs))
		for id := range ao.obs.RemoveChannelIDs {
			first.RemoveChannelIds = append(first.RemoveChannelIds, id)
		}
		slices.Sort(first.RemoveChannelIds)
	}
	if len(ao.obs.UpdateChannelDefinitions) > 0 {
		first.UpdateChannelDefinitions = make(map[uint32]*protocol.LLOChannelDefinitionProto, len(ao.obs.UpdateChannelDefinitions))
		for id, cd := range ao.obs.UpdateChannelDefinitions {
			first.UpdateChannelDefinitions[id] = protocol.ChannelDefinitionToProto(cd)
		}
	}

	ids := make([]llotypes.StreamID, 0, len(ao.obs.StreamValues))
	for id, sv := range ao.obs.StreamValues {
		if sv != nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)

	headerSize := proto.Size(header())
	parts := []*protocol.LLOAttributedObservationTelemetry{first}
	cur, size := first, proto.Size(first)
	for _, id := range ids {
		pv, err := protocol.StreamValueToProto(ao.obs.StreamValues[id])
		if err != nil {
			t.lggr.Warnw("Error making attributed observation telemetry", "seqNr", seqNr, "observer", ao.observer, "streamID", id, "err", err)
			continue
		}
		entry := streamValuesEntrySize(id, pv)
		if size+entry > t.maxBytes && size > headerSize {
			cur = header()
			size = headerSize
			parts = append(parts, cur)
		}
		if cur.StreamValues == nil {
			cur.StreamValues = make(map[uint32]*protocol.LLOStreamValue)
		}
		cur.StreamValues[id] = pv
		size += entry
	}
	return parts
}

// streamValuesEntrySize is the serialized size of one stream_values map entry.
func streamValuesEntrySize(id llotypes.StreamID, v *protocol.LLOStreamValue) int {
	n := protowire.SizeTag(1) + protowire.SizeVarint(uint64(id)) +
		protowire.SizeTag(2) + protowire.SizeBytes(proto.Size(v))
	return protowire.SizeTag(9) + protowire.SizeBytes(n)
}
