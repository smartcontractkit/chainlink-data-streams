# zstdenc

A trimmed copy of the zstd encoder from
[klauspost/compress](https://github.com/klauspost/compress) **v1.20.3**, under
its BSD license (see `LICENSE`).

## Why

The v31 precursor is digested and signed, and the KV records feed the state
root, so every oracle must compress them to the same bytes. zstd only promises
that output for one encoder version and one set of options. As a regular
dependency, the encoder version is chosen by the binary that imports this
module, and any unrelated upgrade there can move it. This copy fixes the
encoder to code this repository owns.

Only the encoder is copied. Decoding stays on the upstream module: any
conforming decoder reads these frames, and the decoder parses untrusted peer
input, so it should keep receiving upstream fixes.

## What is kept

Exactly what `NewWriter` with `SpeedDefault`, `WithEncoderCRC(true)`,
`WithZeroFrames(false)` and `WithSingleSegment(true)` followed by `EncodeAll`
links, on every supported build (amd64, arm64, 386, riscv64, and
`purego,noasm,nounsafe`):

| package | upstream |
|---|---|
| `zstd` | `zstd`, the double fast encoder and the block, sequence and FSE encoders |
| `huff0` | `huff0`, the Huffman encoder |
| `fse` | `fse`, the FSE encoder huff0 uses for its weight table |
| `internal/le`, `internal/regmask` | the same internal packages |

`zstd/internal/xxhash` is replaced by `github.com/cespare/xxhash/v2`, the
package it was forked from. XXH64 is fixed by its specification, so the frame
checksum does not depend on that version.

## How it was trimmed

The upstream files were copied with only the import paths rewritten, and then
code was only deleted, never edited:

1. Functions the linker drops from a program that calls only the API above,
   with inlining off, in every build configuration that compiles their file.
2. Types, constants, variables and struct fields nothing references.
3. Files and assembly left with no declarations.
4. The other compression levels from the `encoderOptions.encoder` switch, and
   `ResetPrefix` and `UseBlock` from the `encoder` interface.
5. Compiled out blocks (`if false`, `if debugEncoder`) that call deleted
   functions, in `fse.Compress`, `huff0.Compress4X` and `blockEnc.encode`.
6. The decoder tables from `initPredefined`. The encoder tables copy only the
   normalized counts from them and build their own.

Every remaining declaration is byte for byte identical to upstream, apart from
lines deleted by the steps above and gofmt realigning struct fields. Keep it
that way: a change here changes the bytes every oracle must agree on.

## Verifying and upgrading

`zstdenc_test.go` checks the copy against upstream on a corpus and with a fuzz
target, round trips it through the upstream decoder, and freezes the corpus
output in a digest. The differential tests skip once the upstream module is no
longer v1.20.3. The digest test and the v31 golden tests keep guarding the
bytes after that.

Taking an upstream encoder change means copying and trimming again, and it
changes the encoded bytes, so it needs the same coordinated rollout as any
other wire format change.
