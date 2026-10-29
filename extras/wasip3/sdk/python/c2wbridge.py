"""Client of the container2wasm WASI 0.3 bridge.

Programs running inside a container2wasm WASI 0.3 image use this module to
call the WIT interfaces imported by the component and to serve the
interfaces it exports. It only depends on the standard library.

    import c2wbridge
    bridge = c2wbridge.Client()
    now = bridge.call("wasi:clocks/system-clock@0.3.0", "now")
    bridge.serve("example:greeter/handler@0.1.0", {"greet": lambda name: f"hi {name}"})
    bridge.wait()

The protocol and the mapping between WIT and JSON values are documented in
docs/wasip3-bridge-protocol.md.
"""

import json
import os
import socket
import threading

DEFAULT_SOCKET = "/run/c2w/bridge.sock"


class BridgeError(Exception):
    """Raised when the host or the bridge reports an error."""


class Client:
    def __init__(self, path=None):
        path = path or os.environ.get("C2W_BRIDGE_SOCKET") or DEFAULT_SOCKET
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.connect(path)
        self._file = self._sock.makefile("rb")
        self._write_lock = threading.Lock()
        self._lock = threading.Lock()
        self._pending = {}
        self._next_id = 0
        self._handlers = {}
        self._closed = threading.Event()
        self._error = None
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

    # -- public API -------------------------------------------------------

    def call(self, interface, function, *args):
        """Call `function` of the imported `interface` and return its result."""
        return self._request("call", {"interface": interface, "function": function, "args": list(args)})

    def describe(self):
        """Return the description of the WIT world (wasm-tools JSON)."""
        return self._request("describe", None)

    def serve(self, interface, handlers):
        """Serve the exported `interface`.

        `handlers` maps function names to callables receiving the decoded
        arguments and returning the result.
        """
        self._handlers[interface] = handlers
        return self._request("serve", {"interface": interface})

    def wait(self):
        """Block until the connection is closed."""
        self._closed.wait()
        if self._error:
            raise self._error

    def close(self):
        self._sock.close()

    # -- internals ----------------------------------------------------------

    def _send(self, msg):
        data = (json.dumps(msg) + "\n").encode()
        with self._write_lock:
            self._sock.sendall(data)

    def _request(self, method, params):
        with self._lock:
            self._next_id += 1
            rid = self._next_id
            event = threading.Event()
            self._pending[rid] = [event, None]
        msg = {"id": rid, "method": method}
        if params is not None:
            msg["params"] = params
        self._send(msg)
        event.wait()
        with self._lock:
            _, reply = self._pending.pop(rid)
        if reply is None:
            raise BridgeError("connection closed")
        if reply.get("error") is not None:
            raise BridgeError(reply["error"])
        return reply.get("result")

    def _read_loop(self):
        try:
            for line in self._file:
                msg = json.loads(line)
                if msg.get("method") == "invoke":
                    threading.Thread(target=self._handle_invoke, args=(msg,), daemon=True).start()
                    continue
                with self._lock:
                    entry = self._pending.get(msg.get("id"))
                if entry:
                    entry[1] = msg
                    entry[0].set()
        except Exception as e:  # pylint: disable=broad-except
            self._error = e
        finally:
            with self._lock:
                for event, _ in self._pending.values():
                    event.set()
            self._closed.set()

    def _handle_invoke(self, msg):
        reply = {"id": msg.get("id")}
        try:
            params = msg.get("params") or {}
            handlers = self._handlers.get(params.get("interface"))
            if handlers is None:
                raise BridgeError("no handler for %s" % params.get("interface"))
            fn = handlers.get(params.get("function"))
            if fn is None:
                raise BridgeError("no handler for %s.%s" % (params.get("interface"), params.get("function")))
            reply["result"] = fn(*(params.get("args") or []))
        except Exception as e:  # pylint: disable=broad-except
            reply["error"] = str(e)
        self._send(reply)
