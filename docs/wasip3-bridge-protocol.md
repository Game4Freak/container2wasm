# WASI 0.3 bridge protocol

This document specifies how programs running inside a container2wasm WASI 0.3
image call the WIT interfaces of the component and serve the interfaces it
exports. See [wasip3.md](./wasip3.md) for the architecture.

Two protocols are involved:

1. the **socket protocol** between programs in the container and the bridge
   daemon (`init`): newline delimited JSON on `/run/c2w/bridge.sock`. This is
   what language SDKs implement.
2. the **frame protocol** between the bridge daemon and the emulator over the
   virtio-console port `/dev/hvc1`. Only `init` and the emulator implement it.

Both carry the same JSON documents and share the WIT to JSON mapping below.

## WIT values in JSON

| WIT type | JSON |
| --- | --- |
| `bool` | `true` / `false` |
| `u8` .. `u64`, `s8` .. `s64` | number (SDKs should use arbitrary precision for 64 bit values; decimal strings are accepted as input) |
| `f32`, `f64` | number; `NaN` and infinities as the strings `"nan"`, `"inf"`, `"-inf"` |
| `char` | string containing one Unicode scalar value |
| `string` | string |
| `list<u8>` | base64 string (standard alphabet with padding); an array of numbers is accepted as input |
| `list<T>` | array |
| `tuple<A, B, ...>` | array of fixed length |
| `record` | object keyed by the WIT field names (kebab-case as in WIT) |
| `variant` | `{"tag": "case-name", "val": payload}`; `val` is omitted for cases without payload |
| `enum` | string with the case name |
| `flags` | array of the set flag names |
| `option<T>` | `null` or the value (nested options are not supported) |
| `result<T, E>` | `{"ok": value}` or `{"err": value}`; the value is `null` when the side has no type |

Function results: a function without result maps to `null`.

Interface names are the fully qualified WIT names, including the package
version when the package has one: `example:greeter/host@0.1.0`,
`wasi:clocks/system-clock@0.3.0`. Functions declared directly in the world
(not in an interface) use the empty string as interface name.

## Socket protocol

Transport: unix stream socket `/run/c2w/bridge.sock` (overridable for SDKs
with the `C2W_BRIDGE_SOCKET` environment variable). Each message is one JSON
object terminated by `\n`. Messages follow the JSON-RPC 2.0 shape without the
`jsonrpc` field. Requests may be pipelined; responses carry the request `id`.

### Requests from the program to the bridge

Call an imported function:

```json
{"id": 1, "method": "call", "params": {"interface": "example:greeter/host@0.1.0", "function": "greet", "args": ["world"]}}
{"id": 1, "result": "hello, world!"}
```

Errors (unknown function, conversion failure, host error) are reported as:

```json
{"id": 1, "error": "unknown function example:greeter/host@0.1.0.hello"}
```

Get the description of the world (the output of `wasm-tools component wit
--json`, containing every interface, function and type of the component):

```json
{"id": 2, "method": "describe"}
{"id": 2, "result": {"worlds": [...], "interfaces": [...], "types": [...], "packages": [...]}}
```

Serve an exported interface. From then on the bridge sends `invoke` requests
for that interface to this connection; the last connection to register an
interface serves it, and registrations are dropped when the connection
closes:

```json
{"id": 3, "method": "serve", "params": {"interface": "example:greeter/guest@0.1.0"}}
{"id": 3, "result": true}
```

### Requests from the bridge to the program

After `serve`, calls made by the host arrive as requests on the same
connection. The program answers with a response carrying the same `id`:

```json
{"id": 7, "method": "invoke", "params": {"interface": "example:greeter/guest@0.1.0", "function": "handle", "args": ["ping"]}}
{"id": 7, "result": "PING"}
```

An `error` response (or no handler) is reported to the host: if the function
returns `result<_, string>` the host receives the error as `err`, otherwise
the component traps with the message.

Requests sent by the two sides are distinguished by the presence of
`method`; ids chosen by the bridge never need to be unique with the ids chosen
by the program because responses flow in the opposite direction of their
requests.

## Frame protocol

Transport: the virtio-console port `/dev/hvc1` configured as a raw tty. Every
frame is a 16 byte little-endian header followed by `len` payload bytes:

```
offset  size  field
0       4     magic 0x42573243 ("C2WB")
4       1     version = 1
5       1     type
6       2     reserved (0)
8       4     id
12      4     len (payload size, at most 64 MiB)
```

| type | name | direction | payload |
| --- | --- | --- | --- |
| 1 | HELLO | guest → emulator | empty; sent once when the daemon starts |
| 2 | DESCRIBE | emulator → guest | the world description JSON |
| 3 | CALL | guest → emulator | `{"interface", "function", "args"}`; `id` chosen by the guest |
| 4 | RESULT | emulator → guest | `{"result": value}` or `{"error": message}` for the CALL with the same `id` |
| 5 | INVOKE | emulator → guest | `{"interface", "function", "args"}`; `id` chosen by the emulator |
| 6 | RETURN | guest → emulator | `{"result": value}` or `{"error": message}` for the INVOKE with the same `id` |

Calls in both directions may be outstanding concurrently. The emulator
forwards a CALL to the bridge component as the JSON document
`{"id": <id>, "request": <payload>}` through `c2w:bridge/vm.next-request`
and sends the RESULT when the component calls `c2w:bridge/vm.complete`.
INVOKE frames correspond to `c2w:bridge/vm.invoke` calls.
