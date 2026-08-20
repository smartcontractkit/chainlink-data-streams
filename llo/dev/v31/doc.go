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
// The blob payload itself is framed by blobcompress.go: a leading codec byte
// followed by the stream-values proto, zstd-compressed whenever that shrinks
// it. The codec is chosen by the writer and read from the byte, so nodes need
// not agree on whether compression paid off.
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
// taken from the channel's opts, which is where they are declared), cross-round
// timestamped-aggregate carry-forward (in the r/agg record, newer-wins
// monotonicity), and best-effort outcome/report telemetry are also implemented.
// Reportability is persisted per channel each round (also in r/agg) so the next
// round can advance validAfter faithfully without re-deriving it from
// aggregates that are not otherwise persisted.
//
// Calculated streams (EVMABIEncodeUnpackedExpr channels) are supported via the
// expression engine in llo/protocol/calculated, run at the end of
// StateTransition. A channel whose expressions did not produce every calculated
// stream its opts declare is not reportable, regardless of
// DisableNilStreamValues: the codec would have nothing to encode, so the report
// is skipped, and counting the channel as reported would advance validAfter over
// a round that emitted nothing.
//
// Evaluation writes stream aggregates and nothing else. It does not touch the
// channel definitions, so a persisted definition is exactly what was voted on
// and no derived state reaches the replicated key-value store. Which streams a
// channel reports — its observed streams followed by one calculated stream per
// declared expression, in declaration order — is derived on demand by
// protocol.EffectiveStreams, which is a pure function of the definition and its
// opts and therefore identical on every oracle. Report assembly must go through
// it rather than reading cd.Streams, since the trailing calculated values are
// what ReportCodecEVMABIEncodeUnpackedExpr encodes as its payload.
//
// v3.0 instead appends the calculated streams to the definitions it commits in
// its outcome (calculated.ProcessCalculatedStreamsWithDefinitionAppend), which
// EffectiveStreams drops any such inline entries before appending the declared
// ones, so it reads definitions written by either version identically.
//
// # Stream history
//
// Expressions can read a window of a stream's past agreed values with
// History(s<streamID>, <depth>) (see llo/protocol/calculated). Windows are
// persisted per (streamID, aggregator) pair — the aggregator is part of the
// identity because the same stream may be aggregated differently by different
// channels, and interleaving those series would be silently wrong:
//
//	hh/<streamID BE uint32><aggregator BE uint32>              -> LLOStreamHistoryHeaderProto
//	hc/<streamID BE uint32><aggregator BE uint32><slot BE u32> -> LLOStreamHistoryChunkProto
//	hidx                                                       -> sorted (streamID, aggregator) pairs
//	hv                                                         -> history layout version
//
// A window is a ring of chunks rather than one value: a slot holds
// MaxHistoryChunkRecords records, and a round rewrites only the newest one.
// Sealed chunks are immutable for as long as they are retained and eviction is a
// delete, so a pair's per-round write cost is a function of the chunk size, not
// of its depth — about 2 KiB rather than 60 KiB for a full quote window at
// maximum depth. Reads cost depth/chunkSize point reads instead of one, and only
// the chunks covering what was asked for are read.
//
// The header alone says which chunks are retained, how full each is and when
// each starts, so a round decides whether there is enough depth, which chunks to
// read, whether a value may be appended and which chunk falls out without
// opening a chunk at all. A pair still warming up therefore costs one read.
//
// The ring is a fixed slot space rather than an unbounded sequence because the
// in-round reader has no range scan: if a header cannot be decoded there is no
// way to discover which chunk keys exist, and a bounded space makes recovery a
// blind delete of every slot. A chunk left by an earlier lap carries a sequence
// the header no longer retains, which is how slot reuse stays safe.
//
// hidx exists for the same no-range-scan reason: it is what lets windows for
// pairs no channel references any more be found and deleted. hv records the
// layout; a mismatch drops every stored window and re-warms, which is the whole
// migration story while v31 is under llo/dev.
//
// Per round, in StateTransition:
//
//   - computeHistoryRequirements derives the depth each pair needs (the deepest
//     any live channel's expressions ask for) from the channel definitions and
//     their opts. Both are replicated and expression analysis is a pure function
//     of the expression string, so every oracle computes the same depths — they
//     become persisted state. Pairs beyond MaxHistoryPairs are denied history
//     entirely, in (streamID, aggregator) order; channels reading them do not
//     report, rather than silently evaluating over a shorter window. The pair cap
//     is the only admission rule: per-round cost no longer depends on depth, so
//     MaxHistoryPairs of them fit the byte budget by construction.
//   - aggregate appends each required pair's agreed value, timestamped with the
//     value's own observation time for timestamped aggregates and the round's
//     consensus observation timestamp otherwise. An append only takes effect if
//     it is strictly newer than the newest stored record, which is what stops a
//     carried-forward t/ value from being counted once per round until it
//     refreshes. A pair with no aggregate this round contributes nothing: a gap
//     is honest, a repeated value is not.
//   - ProcessCalculatedStreams reads through the same store. An expression whose
//     window is still shallower than requested is not evaluated and writes no
//     aggregate, so the channel is not reportable and validAfter does not
//     advance. This is the warmup gate, and it means adding a History call to a
//     live channel stops it reporting for as many rounds as the depth requested.
//   - flushKV writes each modified window's header and newest chunk, deletes the
//     chunks that fell out and the pairs no live channel requires, and rewrites
//     hidx at most once.
//
// Each of a pair's keys is read at most once and written at most once per round
// however many channels or expressions reference it, which is what keeps history
// inside the per-round key-value budget. A window that cannot be decoded — bad
// header, missing chunk, or one that does not match the header — is discarded
// whole and re-warmed rather than failing the round.
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
