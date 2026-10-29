#!/bin/bash
# Build the bridge component (extras/wasip3/bridge) for a WIT world.
#
# usage: build-bridge.sh <wit dir> <out dir>
#
# Environment:
#   C2W_SRC         repository root (default: derived from this script)
#   WIT_BRIDGE_GEN  wit-bridge-gen binary (default: built with go)
#   WIT_WORLD       world to bridge when the package defines several
#   CARGO_TARGET_DIR optional cargo cache directory
#
# Outputs in <out dir>:
#   bridge.wasm        the component
#   description.json   WIT description served to the guest
#   crate/             the generated crate (for debugging)
set -euo pipefail
WIT_DIR=$(cd "$1" && pwd)
OUT=$(mkdir -p "$2" && cd "$2" && pwd)
C2W_SRC=${C2W_SRC:-$(cd "$(dirname "$0")/../../.." && pwd)}
WIT_BRIDGE_GEN=${WIT_BRIDGE_GEN:-}
if [ -z "$WIT_BRIDGE_GEN" ]; then
  WIT_BRIDGE_GEN="$OUT/wit-bridge-gen"
  (cd "$C2W_SRC" && go build -o "$WIT_BRIDGE_GEN" ./cmd/wit-bridge-gen)
fi

CRATE="$OUT/crate"
rm -rf "$CRATE"
mkdir -p "$CRATE/src" "$CRATE/wit/deps/app" "$CRATE/wit/deps/c2w-bridge" "$CRATE/wit/deps/wasi-cli-run"
wasm-tools component wit --json "$WIT_DIR" > "$OUT/resolve.json"
"$WIT_BRIDGE_GEN" --resolve "$OUT/resolve.json" ${WIT_WORLD:+--world "$WIT_WORLD"} --out "$OUT/gen"
# The WIT package of the container and its dependencies.
if [ -d "$WIT_DIR/deps" ]; then
  cp -r "$WIT_DIR/deps/." "$CRATE/wit/deps/"
fi
cp "$WIT_DIR"/*.wit "$CRATE/wit/deps/app/"
cp "$C2W_SRC/extras/wasip3/wit/c2w-bridge/"*.wit "$CRATE/wit/deps/c2w-bridge/"
cp "$C2W_SRC/extras/wasip3/bridge/wit/deps/wasi-cli-run/"*.wit "$CRATE/wit/deps/wasi-cli-run/"
cp "$OUT/gen/world.wit" "$CRATE/wit/"
cp "$C2W_SRC/extras/wasip3/bridge/Cargo.toml" "$C2W_SRC/extras/wasip3/bridge/Cargo.lock" "$CRATE/"
cp "$C2W_SRC/extras/wasip3/bridge/src/lib.rs" "$OUT/gen/generated.rs" "$CRATE/src/"
(cd "$CRATE" && cargo build --release --target wasm32-unknown-unknown --locked)
TARGET_DIR=${CARGO_TARGET_DIR:-$CRATE/target}
wasm-tools component new "$TARGET_DIR/wasm32-unknown-unknown/release/c2w_bridge.wasm" -o "$OUT/bridge.wasm"
cp "$OUT/gen/description.json" "$OUT/description.json"
echo "built $OUT/bridge.wasm"
