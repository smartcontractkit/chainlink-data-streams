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
// KeyValueStateReader only supports point Read(key); it has no range scan, so
// the set of live channels is tracked in an explicit index key. See kv.go for
// the key schema. All values written to the KV store MUST be serialized
// deterministically (protobuf with Deterministic:true, or fixed-width
// big-endian integers) because the store is replicated across oracles and any
// divergence halts the protocol.
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
// unreportable), cross-round timestamped-aggregate carry-forward (t/ KV keys,
// newer-wins monotonicity), and best-effort outcome/report telemetry are also
// implemented. Reportability is persisted per channel each round (the r/ KV key)
// so the next round can advance validAfter faithfully without re-deriving it
// from aggregates that are not otherwise persisted.
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
