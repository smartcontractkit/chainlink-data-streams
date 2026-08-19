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
// # Blobs
//
// Observations carry per-stream values. When the serialized stream-value
// payload is large it is disseminated as a blob and referenced by handle in the
// observation, rather than sent inline. See observation.go.
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
// Missing: Blob success path isn't unit-tested: ocr3_1types.BlobHandle has no exported constructor,
// it's in an internal package, so a test can't fabricate a handle.
// Tests cover the offload decision + inline fallback;
// the full broadcast→fetch→merge round-trip needs libocr-provided doubles or an integration test.
package llo
