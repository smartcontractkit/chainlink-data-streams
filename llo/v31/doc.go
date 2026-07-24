// Package llo (import path .../llo/v31) implements the LLO reporting plugin
// against libocr's OCR3.1 interface (offchainreporting2plus/ocr3_1types).
//
// It is a sibling of the OCR3.0 plugin at .../llo/v30. Version-agnostic
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
// Deferred to follow-ups (marked with TODO(v31-parity) in code): calculated
// streams, history-backfill channel selection, seconds-resolution overlap
// prevention, and outcome/report telemetry emission. These are advanced
// features that require careful, separately-reviewed ports of the v30 logic.
//
// Missing: Blob success path isn't unit-tested: ocr3_1types.BlobHandle has no exported constructor,
// it's in an internal package, so a test can't fabricate a handle.
// Tests cover the offload decision + inline fallback;
// the full broadcast→fetch→merge round-trip needs libocr-provided doubles or an integration test.
package llo
