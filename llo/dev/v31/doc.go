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
// A snapshot is therefore gathered one round before it is used. Two separate
// bounds apply to it. MaxSnapshotRounds is local: it decides how stale the
// values may be when this node references them (forSeqNr + MaxSnapshotRounds),
// and is what a report format's staleness budget should be tuned against.
// BlobLifetimeRounds is remote: it is the expiration hint given to the blob
// transport (forSeqNr + BlobLifetimeRounds), deciding how long peers can still
// fetch the blob, and sits BlobFetchMarginRounds beyond the last seqNr at which
// the handle can be referenced. A broadcast that the transport refuses is
// retried inside the cycle (BlobBroadcastAttempts), which is what keeps the
// round trip off the OCR critical path; a retry recomputes the hint from the
// round current at that attempt, so the values stay bounded by MaxSnapshotRounds
// while the blob stays fetchable for the round that will reference it. A wall-clock age check derived from the
// measured round period guards against jitter on top. A round that finds
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
//
// # Blue/green handover from v30
//
// A live DON migrates from the OCR3.0 plugin (llo/v30) to this one without a
// reporting gap by running the two as separate protocol instances and handing
// over through the retirement machinery in llo/protocol and llo/retirement.
// Nothing in that machinery is OCR-version specific: the retirement report is
// the same JSON (protocol.StandardRetirementReportCodec), and OCR3.1 reuses
// ocr3types.OnchainKeyring verbatim, so a v3.1 successor verifies a v3.0
// predecessor's attested report with the same signature scheme, and vice versa.
// llo/dev/v31/handover_test.go drives both plugins through the handover in both
// directions and asserts the report intervals are gapless and non-overlapping
// across the boundary.
//
// The handover is INTRA-JOB. The chainlink LLO delegate already runs blue/green
// as one job with one or two ContractConfigTrackers (index 0 "Blue", index 1
// "Green"), each its own protocol instance with its own config digest, sharing
// one ChannelDefinitionCache, one ShouldRetireCache, one DataSource, one
// telemetry pipeline and one plugin-scoped retirement report cache. A v30 -> v31
// migration is the ordinary blue/green flow with the two instances running
// DIFFERENT plugin versions; it does not need, and should not use, a second job.
//
// Consumer prerequisite (out of this repo): the OCR version must be selected per
// instance rather than per job. As of writing, chainlink resolves it once for the
// whole job (core/services/llo/delegate.go takes a single OCR31 bool, set from
// pluginconfig.PluginConfig.IsOCR31), so both instances necessarily run the same
// plugin. Making it per-instance means:
//
//   - a per-instance version in the job's pluginConfig, aligned with the tracker
//     list and defaulting to the existing scalar ocrVersion for every instance
//     when absent, so current job specs stay valid;
//   - the delegate's oracle-construction loop choosing the OCR3.0 or OCR3.1
//     oracle by instance index;
//   - the OCR3.1-only dependencies (the "2" network endpoint factory and the
//     KeyValueDatabaseFactory) built when ANY instance is 3.1, not when the job
//     is.
//
// Two things need no work: ocr3_1types.KeyValueDatabaseFactory takes the config
// digest (NewKeyValueDatabase(configDigest)), so one factory shared by both
// instances already gives them separate keyspaces; and the OCR3.0 and OCR3.1
// network endpoint factories are independent, so one peer can serve both
// transports at once.
//
// Wiring requirements for the handover itself:
//
//   - The SAME retirement.RetirementReportCache must back both instances. The
//     retiring instance's transmitter writes its attested report into it (keyed
//     by the retiring instance's config digest) and the successor's plugin-scoped
//     cache reads it back. Sharing it is automatic within one job; a successor
//     that starts before the predecessor's ConfigSet row is stored simply cannot
//     verify the report yet and stays in staging.
//   - The successor's retirement.NewPluginScopedRetirementReportCache takes a
//     verifier for the PREDECESSOR's keyring. Because both OCR versions sign
//     reports through ocr3types.OnchainKeyring, the one keyring the job already
//     holds serves both; no version-specific casing is needed.
//
// History-backfill channels need no special handling. Both instances share one
// ChannelDefinitionCache, so a backfill channel is present on the staging
// instance too, where it would be a NEW channel with a watermark of 0 and would
// replay the whole backfill from the beginning for the length of the overlap.
// Both plugins therefore skip backfill entirely while not in the production
// stage: isReportable returns false and the watermark does not advance.
//
// Skipping costs nothing, because promotion seeds ValidAfterNanoseconds
// wholesale from the predecessor's retirement report: a staging instance's
// backfill watermark is discarded unread, so the replay could never have
// counted. The backfill simply pauses for the overlap and resumes at the
// predecessor's position. A backfill channel the predecessor never reported is
// absent from the report and starts from the beginning after promotion, which
// is the correct reading.
//
// Nothing else needs to warm up. Only validAfter watermarks are handed over, and
// that is all a v3.0 predecessor has: v30 passes a nil HistoryReader
// (v30/stream_calculated.go), so History() and TWAP fail closed there and no
// live v3.0 channel can be using them. There are no stream history windows to
// rebuild. Backfill watermarks live in validAfter and so transfer with it.
//
// Operator sequence (v30 -> v31), assuming instance 0 is the live v3.0
// production instance:
//
//  1. Publish a new OCR3.1 config instance on the ConfigurationStore, with
//     onchainConfig.predecessorConfigDigest set to the v3.0 instance's config
//     digest. A staging instance REQUIRES a predecessor; with none it starts
//     straight in production and inherits no watermarks.
//  2. Update the job spec on every node: add the new config tracker as instance
//     1 (Green) and mark instance 1 as OCR version "3.1"
//     (pluginconfig.OCRVersionOCR31) while instance 0 stays "3.0". Green
//     bootstraps into the staging stage.
//  3. Let it run, and verify Green from TELEMETRY, not from the Mercury server.
//     A staging instance marks its reports Specimen = true and the EVM codecs
//     refuse to encode those, so nothing it produces is transmitted and transmit
//     volume does not rise. captureReportTelemetry runs before the encode, so
//     ReportTelemetryCh and OutcomeTelemetryCh do see what Green would have
//     emitted. The readiness gate is Green's report telemetry covering the same
//     channel set as Blue's production output, with sane values, plus the blob
//     pump's Misses/Cycles low and uncorrelated. There is no warm-up minimum to
//     wait out (see above), so the overlap is however many rounds of that
//     evidence you want.
//  4. Vote to retire v3.0: set shouldRetire for the v3.0 config digest in the
//     ConfigurationStore. Once more than f oracles observe it, instance 0 moves
//     to the retired stage in the round it is agreed (retirement is NOT
//     deferred), stops reporting, and emits its retirement report.
//  5. Watch the Green logs for "Promoting protocol instance from staging to
//     production". Its validAfter watermarks are then seeded verbatim from the
//     predecessor's report, so its first production report resumes exactly where
//     v3.0 stopped: one wide report closing the overlap window, no gap and no
//     overlap. Channels the staging instance added itself, which the predecessor
//     never reported, are treated as new (validAfter = the promotion round's
//     observation timestamp) rather than keeping a staging watermark.
//  6. Once the retirement report is stored and Green is producing, drop the
//     retired instance from the job spec, moving the v3.1 instance to position 0
//     so the second slot is free for the next migration.
//
// Rollback is symmetric: add a v3.0 config instance whose
// predecessorConfigDigest is the v3.1 instance's digest, run it as the staging
// instance, and retire v3.1. The v3.1 instance emits the same retirement report
// format, so nothing special is needed on the way back.
//
// Two behaviours to expect while a handover is in flight:
//
//   - An attested retirement report the successor cannot verify (for example
//     because the predecessor's ConfigSet row has not been loaded into the local
//     RetirementReportCache yet) only drops the retirement field from that
//     observation. The round still completes on the rest of it and the instance
//     stays in staging, retrying every round until the report verifies.
//   - Promotion additionally requires the predecessor's report to pass
//     RetirementReport.CheckCompatible: retirement reports carry an LLO protocol
//     version and are not guaranteed to be compatible across LLO protocol
//     versions (as distinct from OCR versions, which are interchangeable here).
//     A report from a version this build does not understand is logged and
//     ignored rather than promoting on watermarks it may have misread.
package llo
