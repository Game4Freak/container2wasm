#!/usr/bin/env python3
"""Runs inside the container: serves example:greeter/guest and calls the host.

Copy extras/wasip3/sdk/python/c2wbridge.py next to this file (or into the
image) before running it.
"""
import c2wbridge

bridge = c2wbridge.Client()
print("host says:", bridge.call("example:greeter/host@0.1.0", "greet", "container"))
print("2 + 3 =", bridge.call("example:greeter/host@0.1.0", "add", 2, 3))


def handle(msg):
    print("host asked:", msg)
    return msg.upper()


bridge.serve("example:greeter/guest@0.1.0", {"handle": handle})
print("serving example:greeter/guest ...")
bridge.wait()
