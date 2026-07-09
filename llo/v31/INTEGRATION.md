# Wiring the LLO OCR3.1 plugin (`llo/v31`)

This plugin implements libocr's **OCR3.1** reporting-plugin interface
(`offchainreporting2plus/ocr3_1types`). The plugin/factory live here; the OCR
oracle is instantiated by the consumer (chainlink-core). This guide covers what
the consumer must change relative to wiring the OCR3.0 (`llo/v30`) plugin.

At a glance, moving from v30 → v31 means:

| Concern | v30 (OCR3.0) | v31 (OCR3.1) |
|---|---|---|
| Oracle args | `offchainreporting2plus.OCR3OracleArgs` | **`OCR3_1OracleArgs`** (or `OCR3_1OracleArgs2` for `OnchainKeyring2`) |
| Replicated KV store | — | **`KeyValueDatabaseFactory` required** |
| Config helper | `ocr3confighelper` | **`ocr3_1confighelper`** (signature change) |
| Blob dissemination | — | injected by the runtime (`BlobBroadcastFetcher`) |
| Plugin factory | `llov30.NewPluginFactory` | `llov31.NewPluginFactory` |
| `DataSource` / `DSOpts` | v30 shape (has `OutCtx`/`OutcomeCodec`) | **v31 shape** (`SeqNr`/`ConfigDigest`/`ObservationTimestamp`/`VerboseLogging`) |
| `ContractTransmitter` | `ocr3types.ContractTransmitter` | **unchanged** — same transmitter |
| Report codecs | `map[ReportFormat]ReportCodec` | **unchanged** (register the same set) |

## 1. Construct the plugin factory

```go
import (
    llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
    llov31 "github.com/smartcontractkit/chainlink-data-streams/llo/v31"
    llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

factory := llov31.NewPluginFactory(llov31.PluginFactoryParams{
    Config:                           llov31.Config{VerboseLogging: false},
    PredecessorRetirementReportCache: predecessorCache, // llocommon.PredecessorRetirementReportCache
    ShouldRetireCache:                shouldRetireCache, // llov31.ShouldRetireCache
    RetirementReportCodec:            llocommon.StandardRetirementReportCodec{},
    ChannelDefinitionCache:           cdc,               // llotypes.ChannelDefinitionCache
    DataSource:                       ds,                // llov31.DataSource (see §5)
    Logger:                           lggr,
    OnchainConfigCodec:               onchainCodec,      // llocommon.OnchainConfigCodec
    ReportCodecs:                     reportCodecs,      // see §4
    OutcomeTelemetryCh:               outcomeTelemCh,    // optional
    ReportTelemetryCh:                reportTelemCh,     // optional
    DonID:                            donID,
    BlobThreshold:                    0,                 // 0 => llov31.DefaultBlobThreshold (128 KiB); <0 disables blob offload
})
```

Most fields are identical to v30's `PluginFactoryParams`. The notable removal is
the **outcome codec** — v31 keeps state in the replicated KeyValueStore instead
of threading an encoded outcome, so there is no `OutcomeCodec`/protocol-version
codec selection to configure.

## 2. Instantiate the OCR3.1 oracle

```go
import (
    "github.com/smartcontractkit/libocr/offchainreporting2plus"
)

oracle, err := offchainreporting2plus.NewOracle(
    offchainreporting2plus.OCR3_1OracleArgs[llotypes.ReportInfo]{
        // ---- unchanged from OCR3OracleArgs ----
        BinaryNetworkEndpointFactory: netFactory,
        V2Bootstrappers:              bootstrappers,
        ContractConfigTracker:        tracker,
        ContractTransmitter:          transmitter, // ocr3types.ContractTransmitter[llotypes.ReportInfo]
        Database:                     ocrDB,        // ocr3_1types.Database
        LocalConfig:                  localConfig,
        Logger:                       lggr,
        MonitoringEndpoint:           monEndpoint,
        OffchainConfigDigester:       digester,
        OffchainKeyring:              offKeyring,
        OnchainKeyring:               onKeyring,    // ocr3types.OnchainKeyring[llotypes.ReportInfo]

        // ---- new / changed for OCR3.1 ----
        KeyValueDatabaseFactory: kvdbFactory, // REQUIRED, see §3
        ReportingPluginFactory:  factory,     // ocr3_1types.ReportingPluginFactory[llotypes.ReportInfo]
    },
)
```

Note: the `BlobBroadcastFetcher` is **not** an oracle-args field — the runtime
injects it into `NewReportingPlugin` and into the plugin callbacks. The consumer
does not construct it.

## 3. Provide a `KeyValueDatabaseFactory` (new, required)

v31 stores replicated per-round state in the KeyValueStore. The consumer must
supply a **persistent, config-digest-keyed** database implementing
`ocr3_1types.KeyValueDatabaseFactory` (see `ocr3_1types/kvdb.go`):

```go
type KeyValueDatabaseFactory interface {
    NewKeyValueDatabase(configDigest types.ConfigDigest) (KeyValueDatabase, error)
    NewKeyValueDatabaseIfExists(configDigest types.ConfigDigest) (KeyValueDatabase, error)
}
```

`KeyValueDatabase` must provide read and read-write transactions with a `Range`
iterator (`NewReadWriteTransaction`/`NewReadTransaction`, plus `Read`/`Write`/
`Delete`/`Range`/`Commit`). Requirements:

- **Durable** and scoped **per `configDigest`** (each protocol instance has its own store).
- Correctly transactional: a `StateTransition` round's writes must be committed atomically.
- The store's *committed contents* are what must agree across oracles — the plugin
  only writes deterministic bytes, so a faithful KV implementation preserves consensus.

libocr provides an in-memory implementation for tests; production needs a
persistent one (e.g. Postgres/pebble-backed).

## 4. Register report codecs (unchanged, but note backfill)

Register the same `map[llotypes.ReportFormat]llocommon.ReportCodec` as v30. Because
v31 supports history-backfill channels, include the backfill codec **and** the
target formats it delegates to:

```go
reportCodecs := map[llotypes.ReportFormat]llocommon.ReportCodec{
    llotypes.ReportFormatJSON:                    llocommon.JSONReportCodec{},
    llotypes.ReportFormatEVMPremiumLegacy:        evmPremiumLegacyCodec,
    llotypes.ReportFormatEVMABIEncodeUnpacked:    evmUnpackedCodec,
    llotypes.ReportFormatEVMABIEncodeUnpackedExpr: evmUnpackedExprCodec,
    llotypes.ReportFormatHistoryBackfill:         llocommon.ReportCodecHistoryBackfill{},
    // retirement reports are diverted to the RetirementReportCache, not a codec here
}
```

Backfill reports are encoded with the **target** channel's codec, so the target
formats must be registered even if no live channel currently uses them.

## 5. Adapt the `DataSource`

v31 defines its own `DataSource`/`DSOpts` (it does **not** reuse v30's, which
exposed an `OutcomeContext` and `OutcomeCodec` that no longer exist):

```go
type DataSource interface {
    Observe(ctx context.Context, streamValues llocommon.StreamValues, opts DSOpts) error
}
type DSOpts interface {
    VerboseLogging() bool
    SeqNr() uint64
    ConfigDigest() ocrtypes.ConfigDigest
    ObservationTimestamp() time.Time
}
```

An existing v30 `DataSource` implementation must be adapted to this interface. If
your data source only used `opts.SeqNr()`/`ConfigDigest()`/`ObservationTimestamp()`/
`VerboseLogging()` the change is mechanical; if it read `OutCtx()` or
`OutcomeCodec()`, that state is no longer available (previous state lives in the
KV store, which the data source does not see).

## 6. On-chain config (`ocr3_1confighelper`)

Use `ocr3_1confighelper` instead of `ocr3confighelper`. The signature changed:
the leading `bool` became a typed level.

```go
pub, err := ocr3_1confighelper.PublicConfigFromContractConfig(
    ocr3_1confighelper.CheckPublicConfigLevelDefault, // was: a bool
    contractConfig,
)
// tests: ocr3_1confighelper.ContractSetConfigArgsForTests(CheckPublicConfigLevelDefault, ...)
//        ocr3_1confighelper.ContractSetConfigArgsDeterministic(CheckPublicConfigLevelDefault, ...)
```

The ConfigurationStore / on-chain config must use the OCR3.1 config format. There
is no `...MercuryV02` variant in the 3.1 helper.

## 7. Offchain config

v31 reuses `LLOOffchainConfigProto` (`llocommon.OffchainConfig`): `ProtocolVersion`,
`DefaultMinReportIntervalNanoseconds`, `EnableObservationCompression`. Semantics
in v31:

- Internal timestamps are always nanosecond-resolution (no protocol-version-0
  seconds truncation). Report-format-level seconds resolution (EVM premium legacy;
  EVM ABI unpacked with `TimeResolution: "s"`) is still honored to prevent
  overlapping reports.
- `DefaultMinReportIntervalNanoseconds` and `EnableObservationCompression` behave as in v30.

## 8. Rollout / handover

v30 and v31 run as **separate protocol instances** (distinct config digests and
OCR versions). A v31 **staging** instance can hand over from a v30 **production**
predecessor: it reads the predecessor's attested retirement report via
`PredecessorRetirementReportCache` and seeds `ValidAfterNanoseconds` from it for a
gapless transition. Both versions use the shared `StandardRetirementReportCodec`,
so the report is decodable across the boundary.

Caveat: `RetirementReport` carries a `ProtocolVersion` and is documented as "not
guaranteed compatible across protocol versions." Validate the `ValidAfterNanoseconds`
handover on a testnet before relying on a cross-version (v30→v31) promotion.

## 9. Transmitter (unchanged)

The transmitter (`llo/transmitter`, `llo/transmitter/de`, `llo/cre`) implements
`ocr3types.ContractTransmitter[llotypes.ReportInfo]`, which is identical in OCR3.1.
Reuse it as-is. Retirement reports continue to be diverted to the
`RetirementReportCache` rather than transmitted.

## 10. Operational notes

- **Blob store sizing.** With blob offload enabled (default), large observations
  (> `BlobThreshold`, default 128 KiB) are disseminated as blobs. The plugin sets
  conservative blob limits in `ReportingPluginInfo`; the runtime enforces them.
- **KV growth.** State is per-channel (`c/`, `v/`, `r/` keys) plus carry-forward
  timestamped aggregates (`t/`). The plugin deletes keys on channel removal, so a
  correct `KeyValueDatabase` will not grow unbounded, but monitor store size.
- **Determinism dependency.** `StateTransition` fetches blobs for observations
  that reference them; it relies on libocr's guarantee that a validated
  observation's blob remains fetchable during `StateTransition` on every honest
  oracle. This is inherent to the OCR3.1 blob design.
```
