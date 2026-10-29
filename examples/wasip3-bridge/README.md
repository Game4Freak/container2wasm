# Custom WIT interfaces with a WASI 0.3 image

This example converts a container to a WASI 0.3 component that imports
`example:greeter/host` and exports `example:greeter/guest`
([`wit/world.wit`](./wit/world.wit)), and shows both directions of the bridge:

- a Python program inside the container calls `greet` and `add` on the host
  and serves `guest.handle`;
- a wasmtime host written in Rust provides `host` and calls `guest.handle`.

See [`docs/wasip3.md`](../../docs/wasip3.md) for the details.

## Build the image

The container needs the Python SDK and the guest program
([`guest/Dockerfile`](./guest/Dockerfile) copies both):

```console
$ cp ../../extras/wasip3/sdk/python/c2wbridge.py guest/
$ docker build -t greeter-guest ./guest
$ c2w --target=wasi-p3 --wit ./wit --assets=../.. greeter-guest out.wasm
```

## Run it

The wasmtime CLI cannot provide custom imports, so a small host program is
needed. [`host/`](./host/) uses the `wasmtime` crate:

```console
$ cd host && cargo build --release
$ ./target/release/wasip3-bridge-host ../out.wasm
host says: hello, container!
2 + 3 = 5
serving example:greeter/guest ...
host asked: ping from host
host: guest replied "PING FROM HOST"
```

Without the exported interface, `wasmtime run -Sp3 out.wasm` works directly;
functions of standard WASI interfaces imported by the world (for example
`wasi:clocks/system-clock@0.3.0`) are then callable from the container with
`/run/c2w/c2w-call` or the SDKs.

## Other languages

Any language can use the bridge by speaking newline delimited JSON on
`/run/c2w/bridge.sock`, see
[`docs/wasip3-bridge-protocol.md`](../../docs/wasip3-bridge-protocol.md).
A Go SDK is available in
[`extras/wasip3/sdk/go`](../../extras/wasip3/sdk/go/).
