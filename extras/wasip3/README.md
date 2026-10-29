# WASI 0.3 support

Sources specific to the `--target=wasi-p3` output. See
[`docs/wasip3.md`](../../docs/wasip3.md) for the architecture and
[`docs/wasip3-bridge-protocol.md`](../../docs/wasip3-bridge-protocol.md) for
the protocol used by programs running inside the container.

| Directory | Content |
| --- | --- |
| `wit/c2w-bridge/` | `c2w:bridge/vm`, the interface between the emulator core module and the bridge component |
| `wit/default/` | the (empty) world used when `c2w --wit` is not given |
| `vmcore/` | runtime of the emulator core module: `run` task driven by Asyncify, `next-request`/`invoke` tasks woken by futures, frame protocol, embedded pack files |
| `vmcore/test/` | a fake emulator and a wasmtime host exercising the whole chain without booting Linux (`build.sh`, then `host/`) |
| `bochs/` | the Bochs bridge device (`wasm_bridge.cc`) and the patch applied to the Bochs fork |
| `bridge/` | the Rust crate of the bridge component; `src/generated.rs` comes from `cmd/wit-bridge-gen` |
| `sdk/go/`, `sdk/python/` | client libraries for programs running inside the container |
| `scripts/` | build scripts shared by the Dockerfile and local builds |

The guest side (bridge daemon and `c2w-call`) lives in
[`cmd/init/bridge.go`](../../cmd/init/bridge.go).
