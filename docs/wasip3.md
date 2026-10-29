# WASI 0.3 images (`--target=wasi-p3`)

container2wasm can convert a container into a [WASI 0.3](https://wasi.dev/releases/wasi-p3)
component. Compared to the default WASI preview 1 output, a WASI 0.3 image

- is a component (component model, canonical ABI) and runs on WASI 0.3 runtimes
  such as wasmtime >= 46 (`wasmtime run -Sp3 out.wasm`);
- uses the native async ABI of the component model: the emulator runs as an
  async task that yields cooperatively, so the host can call into the
  container and the container can call host functions while the guest is
  running;
- can be given **custom WIT interfaces**: functions imported by the component
  are callable from programs running inside the container, and functions
  exported by the component are served by programs running inside the
  container. Any language can use them through a small JSON protocol on a
  unix socket (see [wasip3-bridge-protocol.md](./wasip3-bridge-protocol.md)).

This is experimental. Only x86_64 containers (Bochs) are supported.

```console
$ c2w --target=wasi-p3 alpine:3.20 out.wasm
$ wasmtime run -Sp3 out.wasm uname -a
Linux localhost 6.1.0 #1 PREEMPT_DYNAMIC ... x86_64 Linux
$ wasmtime run -Sp3 --dir /tmp/share::/mnt/share out.wasm cat /mnt/share/from-host
```

> NOTE: until a release of container2wasm includes this feature, pass
> `--assets=<path to this repository>` to `c2w` so that the build uses the
> `init` program and the WASI 0.3 sources of this checkout.

## Custom interfaces

Write a WIT package describing what the container may call (imports) and what
it provides (exports), and pass its directory with `--wit`:

```wit
// wit/world.wit
package example:greeter@0.1.0;

interface host {
    greet: async func(name: string) -> string;
}

interface guest {
    handle: async func(msg: string) -> string;
}

world app {
    import host;
    export guest;
    import wasi:clocks/system-clock@0.3.0;  // standard WASI interfaces work too
}
```

```console
$ c2w --target=wasi-p3 --wit ./wit alpine:3.20 out.wasm
```

The resulting component imports `example:greeter/host` and
`wasi:clocks/system-clock` and exports `example:greeter/guest` (in addition to
the WASI interfaces needed by the emulator and `wasi:cli/run`).

Inside the container, the bridge is available as a unix socket at
`/run/c2w/bridge.sock` and a small command line client at
`/run/c2w/c2w-call`:

```console
/ # /run/c2w/c2w-call example:greeter/host@0.1.0 greet '["world"]'
"hello, world!"
/ # /run/c2w/c2w-call wasi:clocks/system-clock@0.3.0 now
{"nanoseconds":123456789,"seconds":1761580800}
```

SDKs for [Go](../extras/wasip3/sdk/go/) and [Python](../extras/wasip3/sdk/python/)
wrap the socket protocol; any language can implement it directly. Serving an
exported interface from the container (Python):

```python
import c2wbridge
bridge = c2wbridge.Client()
bridge.serve("example:greeter/guest@0.1.0", {"handle": lambda msg: msg.upper()})
bridge.wait()
```

On the host, the component is instantiated like any other WASI 0.3 component.
[`examples/wasip3-bridge`](../examples/wasip3-bridge/) contains a complete
example with a wasmtime host written in Rust.

Rules and limitations of the bridge:

- Exported functions must be declared `async` in WIT: the container answers
  asynchronously. Imported functions can be sync or async.
- Supported types: all primitives, `string`, `list`, `tuple`, `record`,
  `variant`, `enum`, `flags`, `option` (not nested) and `result`.
  Resources, `stream`, `future` and `error-context` are not supported yet;
  functions using them are skipped (imports) or rejected (exports).
- The WIT package must define exactly one world, or `--build-arg
  BRIDGE_WIT_WORLD=<name>` must select it.
- The description of the world (the JSON produced by `wasm-tools component
  wit --json`) is available to programs in the container
  (`c2w-call --describe`), which lets SDKs validate or generate code.

## Current limitations

- No Wizer snapshot: WASI 0.3 images boot Linux at every start (the
  preview 1 images resume a pre-booted snapshot), so startup takes longer.
- No networking (`--net`) yet. Networking is expected to be provided through
  WASI 0.3 sockets or a custom bridged interface in the future.
- Only wasmtime has been tested as a WASI 0.3 host.

## How it works

```
 host (wasmtime, ...)
   │ wasi:cli/run, wasi:*@0.3.0, <custom imports>        <custom exports>
   ▼                                                              ▲
 ┌──────────────────────────── out.wasm (component) ─────────────┴──┐
 │  bridge component (Rust, generated)                                │
 │   - exports wasi:cli/run and the custom exports                    │
 │   - imports the custom imports and c2w:bridge/vm                   │
 │       │ c2w:bridge/vm: run / next-request / complete / invoke      │
 │       ▼                                                            │
 │  emulator core module (Bochs, wasm32-wasip3)                       │
 │   - `run`: stackless async export driven by Asyncify unwind/rewind │
 │   - virtio-console port carrying the frame protocol                │
 │       │ /dev/hvc1                                                  │
 │       ▼                                                            │
 │  Linux guest: init bridge daemon ─ /run/c2w/bridge.sock ─ container│
 └────────────────────────────────────────────────────────────────────┘
```

The build (`Dockerfile`, stages `p3-*` and `bochs-dev-p3`) has three parts:

1. **Bridge component** (`extras/wasip3/bridge`, `cmd/wit-bridge-gen`). The
   generator reads the WIT package (`wasm-tools component wit --json`) and
   emits Rust code: a `dispatch` function calling the imported functions from
   JSON arguments, implementations of the exported interfaces that forward
   calls to the guest, and JSON conversions for every WIT type. The crate is
   built for `wasm32-unknown-unknown` with wit-bindgen's async support and
   turned into a component. Its `wasi:cli/run` spawns a pump that fetches
   requests from the emulator (`next-request`), performs the host calls
   concurrently and completes them (`complete`), while awaiting the emulator's
   `run`.

2. **Emulator core module** (`extras/wasip3/vmcore`, `extras/wasip3/bochs`).
   Bochs is compiled with wasi-sdk 34 for `wasm32-wasip3` (reactor model) and
   exports `c2w:bridge/vm`:
   - `run` is a stackless async export. Bochs' main loop is a deep synchronous
     C++ stack, so a device timer regularly calls `c2w_vm_yield()`, which
     unwinds the whole emulator stack with Asyncify (the module is already
     asyncified for setjmp/longjmp) and returns the `YIELD` callback code;
     the runtime later invokes the callback, which rewinds the stack and
     resumes emulation. The emulator runs on a private shadow stack so that
     the unwound frames survive while wasi-libc recycles the task's stack.
   - `next-request` and `invoke` are stackless async exports that wait on a
     `future<u8>` which the emulator writes when the matching frame arrives
     from the guest; `complete` is a plain synchronous export that queues a
     frame.
   - The pack files (BIOS, bochsrc, boot.iso, rootfs.bin) are embedded with
     `.incbin` and served through `--wrap`ped POSIX file functions
     (`c2w_pack.c`); wasi-vfs is not used.
   `wac plug` composes the bridge (socket) with the emulator (plug).

3. **Guest side** (`cmd/init/bridge.go`). The emulator adds a second
   virtio-console (`/dev/hvc1`) and writes `br: hvc1` into the boot info.
   `init` then speaks the frame protocol on that port, serves the unix socket
   and bind mounts `/run/c2w` (socket) and `/run/c2w/c2w-call` (a symlink of
   `init`) into the container.

`extras/wasip3/vmcore/test` contains a fake emulator that exercises the whole
chain (yield, futures, generator, composition) without booting Linux; see its
`build.sh` and `host/`.
