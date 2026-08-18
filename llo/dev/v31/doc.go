// Package llo (import path .../llo/dev/v31) implements the LLO reporting plugin
// against libocr's OCR3.1 interface (offchainreporting2plus/ocr3_1types).
//
// # Experimental
//
// This package lives under llo/dev and is experimental: OCR3.1 is not released,
// the state model is still moving, and the API carries no stability guarantee.
// See the llo/dev package documentation. It graduates to .../llo/v31 once the
// protocol version ships.
//
// It is a dev-tree counterpart of the production OCR3.0 plugin at
// .../llo/v30, not a peer of it. Version-agnostic
// primitives (stream values, report codecs, aggregators, channel-definition
// helpers, opts cache, retirement types, lifecycle constants, limits and all
// generated protobuf types) live in the root llo package and are shared via a
// dot-import. This package is a self-contained plugin driver: it does not
// import v30.
//
// # State model
//
// Unlike v30 (which threads the full outcome through OutcomeContext.PreviousOutcome),
// v31 stores state in the replicated KeyValueState. The in-round
// KeyValueStateReader only supports point Read(key); it has no range scan.
//
// State is split by write frequency rather than spread over per-channel and
// per-stream keys, so a round touches a constant number of keys regardless of
// how many channels and streams exist:
//
//   - r/agg holds the per-round ("hot") state — observation timestamp,
//     validAfter watermarks, per-channel reportability, and carry-forward
//     timestamped aggregates — and is rewritten every round.
//   - c/defs holds every channel definition and is rewritten only when the
//     definitions change; c/seqnr records the sequence number of that write.
//   - c/lifecycle holds the lifecycle stage and is written only on change.
//
// Because c/defs is a pure function of c/seqnr, the plugin keeps the decoded
// definitions in memory (channelCache) and re-reads them only at startup or
// when the stored sequence number differs from the cached one. See kv.go.
//
// Only aggregates that must survive across rounds (TimestampedStreamValues) are
// persisted; regular aggregates are recomputed fresh each round and reach
// Reports through the precursor.
//
// All values written to the KV store MUST be serialized deterministically
// (protobuf with Deterministic:true and repeated fields sorted by key — not
// proto maps — or fixed-width big-endian integers) because the store is
// replicated across oracles and any divergence halts the protocol.
//
// # Deferred channel definitions
//
// Channel definition changes agreed in a round take effect in the NEXT round.
// StateTransition carries two sets: the effective set (what the previous round
// committed, i.e. exactly what Observation read and gathered stream values for)
// drives aggregation, calculated streams, reportability, validAfter and the
// precursor; the pending set (effective plus this round's agreed additions,
// updates, removals and tombstones) is what is persisted to c/defs.
//
// This keeps the observed stream values, the channel definitions and the
// decoded channel opts consistent across Observation, StateTransition and
// Reports, so no report is ever encoded under a definition, or with opts, that
// the observations behind it did not match. The decoded channel opts are a pure
// projection of the definitions record, so the two are cached together as one
// immutable protocol.ChannelGeneration per c/seqnr: a round reads opts from the
// generation its own state load resolved and nothing can repoint it. This
// matters because OCR3.1 runs Observation, StateTransition and Reports in
// separate goroutines, so rounds overlap - a StateTransition for seqNr N+1 may
// run while Reports for N is still encoding an older record. Reports has no
// KeyValueStateReader, so it resolves its generation from the precursor's
// c/seqnr instead, which also rebuilds it when a restart lands between
// StateTransition and Reports. A round in which the definitions did not change
// reuses the memoized generation and walks no channels at all.
//
// The cost is one round of latency per change: a channel added at round N is in
// effect at N+1 and first reportable at N+2, and a channel removed or
// tombstoned at N still reports at N. This differs from v30, which applies
// definition changes within the round that agrees them. Lifecycle changes are
// NOT deferred: retirement stops reporting in the round it is agreed.
//
// # Blobs and the blob pump
//
// Stream values are always disseminated as a blob, never inline: an observation
// carries only votes, the retirement report, its timestamp, and the handle of a
// blob holding the round's stream values. See observation.go for the framing.
//
// Gathering those values is off the OCR critical path. blobpump.go runs
// DataSource.Observe in a background loop, serializes the result, broadcasts it,
// and parks the marshaled handle; Observation publishes the round context
// (stream set, seqNr, lifecycle stage) and picks up whatever is parked. Cadence
// is consumption-driven: a cycle is kicked whenever Observation takes or
// discards a snapshot, so the pump rate tracks the round rate without knowing
// deltaRound, and cycles are serial so only one Observe is ever in flight.
//
// A snapshot is therefore gathered one round before it is used, and its
// usability is bounded by the blob expiration hint (forSeqNr +
// BlobLifetimeRounds) plus a generous wall-clock age check. A round that finds
// nothing usable — cold start, a failed cycle, or a stale snapshot — emits an
// observation with no stream values. That is not a halt: quorum counts
// observations, not values. The cost lands in aggregation, which needs >F values
// per stream, so streams miss the round only if more than F nodes miss together.
// The pump counts misses and cycles (Misses / Cycles) so correlated misses show
// up as more than an unexplained report gap.
//
// # Parity status (vs v30)
//
// Implemented: lifecycle bootstrap/transitions (staging→production promotion,
// retirement), channel add/remove voting, stream aggregation (median/mode/quote
// via the shared aggregators), min-report-interval validAfter, precursor
// construction, report generation, blob-backed observations.
//
// Seconds-resolution overlap prevention (for report formats that encode
// timestamps at second granularity) is implemented in resolution.go and applied
// in both the current-round (isReportable) and previous-round (prevReportable)
// reportability checks.
//
// DisableNilStreamValues (a channel with any nil stream aggregate is
// unreportable; for expression channels the expected calculated streams are
// taken from the channel's opts rather than from the definition, since the
// definition only lists them once evaluation succeeded), cross-round
// timestamped-aggregate carry-forward (in the r/agg record, newer-wins
// monotonicity), and best-effort outcome/report telemetry are also implemented.
// Reportability is persisted per channel each round (also in r/agg) so the next
// round can advance validAfter faithfully without re-deriving it from
// aggregates that are not otherwise persisted.
//
// Calculated streams (EVMABIEncodeUnpackedExpr channels) are supported via the
// expression engine in calculated.go, run at the end of StateTransition.
//
// History-backfill channels are supported: backfill.go selects the next
// observation to emit (advancing a per-channel watermark stored in validAfter),
// reportability and validAfter advancement account for it, and Reports emits the
// backfill report encoded with the target channel's codec.
//
// v31 now covers the full v30 reporting-plugin feature set. Consensus-affecting
// logic (state transition, aggregation, reportability, backfill, calculated
// streams) is ported from v30; the transport differs (KV state + blobs).
//
// Blob test coverage: ocr3_1types.BlobHandle has no exported constructor (it
// lives in an internal package), but a handle can be UnmarshalBinary'd from a
// syntactically valid encoding. The llotest subpackage exports an in-memory,
// content-addressed BlobBroadcastFetcher built that way, so the
// broadcast→fetch→merge round trip is unit-tested; only the real libocr
// certification of a blob is out of reach without an integration test. Hosts
// that run this plugin outside libocr (benchmarks, simulation harnesses) must
// pass llotest.NewBlobBroadcastFetcher() rather than a nil fetcher: with a nil
// fetcher the pump stays inert and no observation ever carries stream values.
package llo
