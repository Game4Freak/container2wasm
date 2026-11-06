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

## Networking

WASI 0.3 images always have a network interface, and the host decides what
it may reach: the component imports `wasi:sockets@0.3.0` (TCP sockets and
name lookup) and nothing else is needed on the host side.

```console
$ wasmtime run -Sp3 -Sinherit-network -Sallow-ip-name-lookup out.wasm wget -q -O - https://example.com/
```

A host written with `wasmtime-wasi` grants the same with
`WasiCtxBuilder::inherit_network()`, `allow_tcp(true)` and
`allow_ip_name_lookup(true)` (TCP is disabled by default in the builder);
`socket_addr_check` restricts the addresses the container may connect to.
Without the grants, connections fail inside the container (the host sees
nothing special).

How it works: the guest's virtio-net device is wired to a user-space TCP/IP
stack ([smoltcp](https://github.com/smoltcp-rs/smoltcp)) running in the
bridge component (`extras/wasip3/bridge/src/netstack.rs`). The guest lives
on `10.0.2.0/24` with the static address `10.0.2.15` configured by `init`
(no DHCP):

| address | role |
| --- | --- |
| `10.0.2.15` | the guest |
| `10.0.2.2` | the stack: default gateway, answers ARP and ping |
| `10.0.2.3` | DNS server: `A` queries are resolved with `wasi:sockets/ip-name-lookup`, other types get an empty answer |

Every TCP connection the guest opens (to any address) terminates in the
stack, which connects to the same address and port with a `wasi:sockets`
TCP socket of the host and copies data in both directions, including
half-closes. If the host connection fails the guest connection is reset.
What is not supported yet: UDP other than DNS, inbound connections
(listening in the guest), IPv6 (the guest has no IPv6 address, and `AAAA`
queries return no records), and the `--net`/`--mac` runtime flags of the
preview 1 images. The addresses are constants in `netstack.rs` and
`cmd/init/main.go`.

## Current limitations

- No Wizer snapshot: WASI 0.3 images boot Linux at every start (the
  preview 1 images resume a pre-booted snapshot), so startup takes longer
  (a couple of minutes for alpine under Bochs).
- Networking covers outgoing TCP and DNS only (see [Networking](#networking)):
  no UDP besides DNS, no inbound connections, no IPv6, no ICMP beyond the
  gateway.
- Interactive stdin is unreliable: wasi-libc's non-blocking reads of stdin
  block on WASI 0.3 and report end of file while the host keeps stdin open.
  Hosts driving the container through exported interfaces should not give
  the component a stdin (see `examples/wasip3-bridge/host`).
- Only wasmtime has been tested as a WASI 0.3 host.

## Behaviour of the bridge worth knowing

- A call made by the host to an exported interface before a program in the
  container has registered for it is held by the bridge daemon until a
  program serves the interface (the container usually boots while the host
  already runs). It fails with an error once the container has exited.
- Calls made by the container to the host are answered in the order the
  host completes them; several may be in flight.
- The emulator module keeps its name section, so traps are reported by
  wasmtime with readable backtraces (`bochs!...`).
- Host directories (`wasmtime run --dir host::guest`, or a preopen given by
  the host program) are mounted in the container at the guest path as with
  the preview 1 images; files and directories can be created, listed,
  renamed and removed through them. WASI has no ownership or permission
  bits: entries are reported readable and executable by everyone (so that
  programs installed there run), and `chmod`/`chown` succeed without
  effect. File locks are always granted: WASI has none, and the container
  is the only user of the device.

## Debugging

- `C2W_BRIDGE_DEBUG=1` in the component's environment (`wasmtime run --env`)
  makes the emulator log every bridge frame and the exit of the VM to
  stderr; `c2w --debug-image` enables Bochs' own logging and a verbose
  kernel.
- The emulator module keeps its name section: traps are reported by
  wasmtime as `bochs!<function>` backtraces. Do not use
  `WASMTIME_BACKTRACE_DETAILS=1`: the DWARF of wasi-libc is stale after
  optimization and mislabels every frame.
- `WASMTIME_LOG=wasmtime_wasi=trace` prints every WASI call made by the
  component with its arguments; a guest that is idle shows up as a stream
  of `monotonic-clock.wait-for` calls (Bochs sleeps to keep emulated time
  from running ahead of real time), not as a hang.
- `extras/wasip3/vmcore/test` runs the bridge, the VM core runtime and a
  fake emulator under wasmtime in about a minute; use it before rebuilding
  an image for a change in `extras/wasip3/vmcore`.

## How it works

```
 host (wasmtime, ...)
   │ wasi:cli/run, wasi:*@0.3.0, <custom imports>        <custom exports>
   ▼                                                              ▲
 ┌──────────────────────────── out.wasm (component) ─────────────┴──┐
 │  bridge component (Rust, generated)                                │
 │   - exports wasi:cli/run and the custom exports                    │
 │   - imports the custom imports, wasi:sockets and c2w:bridge/vm     │
 │   - TCP/IP stack (smoltcp) behind the guest's network interface    │
 │       │ c2w:bridge/vm: run / next-request / complete / invoke /    │
 │       │                net-recv / net-send                         │
 │       ▼                                                            │
 │  emulator core module (Bochs, wasm32-wasip3)                       │
 │   - `run`: stackless async export driven by Asyncify unwind/rewind │
 │   - virtio-console port carrying the frame protocol, virtio-net    │
 │       │ /dev/hvc1                    │ eth0 (ethernet frames)      │
 │       ▼                              ▼                             │
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
   `run`. `wasi:cli/run` also starts the network stack (`netstack.rs`): a
   task receives the guest's ethernet frames (`net-recv`), a driver polls
   smoltcp and queues the frames for the guest (`net-send`), and each TCP
   connection accepted from the guest gets two tasks copying data from and
   to a `wasi:sockets` TCP socket of the host.

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
     `exit()` is wrapped at link time: Bochs ends with `exit(0)` when the
     guest powers off, and the wrapper unwinds the emulator with the same
     Asyncify longjmp so that `run` returns the exit code instead of the
     instance being torn down by `wasi:cli/exit`.
   - `next-request` and `invoke` are stackless async exports that wait on a
     `future<u8>` which the emulator writes when the matching frame arrives
     from the guest; `complete` is a plain synchronous export that queues a
     frame.
   - The pack files (BIOS, bochsrc, boot.iso, rootfs.bin) are embedded with
     `.incbin` and served through `--wrap`ped POSIX file functions
     (`c2w_pack.c`); wasi-vfs is not used. The pack directory itself can be
     stat'ed (the 9p device that exposes the VM info file to the guest
     needs that) but not listed.
   `wac plug` composes the bridge (socket) with the emulator (plug).

3. **Guest side** (`cmd/init/bridge.go`). The emulator adds a second
   virtio-console (`/dev/hvc1`) and writes `br: hvc1` into the boot info.
   `init` then speaks the frame protocol on that port, serves the unix socket
   and bind mounts `/run/c2w` (socket) and `/run/c2w/c2w-call` (a symlink of
   `init`) into the container.

`extras/wasip3/vmcore/test` contains a fake emulator that exercises the whole
chain (yield, futures, generator, composition) without booting Linux; see its
`build.sh` and `host/`.
