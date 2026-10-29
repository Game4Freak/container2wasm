#!/bin/bash
# Build the fake VM test component.
#
# Environment:
#   WASI_SDK_PATH   wasi-sdk >= 34 (wasm32-wasip3 target)
#   BINARYEN_PATH   directory containing wasm-opt
#   WIT_BRIDGE_GEN  path to the wit-bridge-gen binary (default: go run)
#   JMP_SRC         path to Bochs' wasi_extra/jmp (default: fetched from the fork)
#   OUT             output directory (default: ./out)
#
# Produces $OUT/composed.wasm, to be run with host/ (cargo run -- composed.wasm).
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../../../.. && pwd)
OUT=${OUT:-$PWD/out}
WASI_SDK_PATH=${WASI_SDK_PATH:?set WASI_SDK_PATH}
BINARYEN_PATH=${BINARYEN_PATH:?set BINARYEN_PATH}
WIT_BRIDGE_GEN=${WIT_BRIDGE_GEN:-}
BOCHS_REPO=${BOCHS_REPO:-https://github.com/ktock/Bochs}
BOCHS_REPO_VERSION=${BOCHS_REPO_VERSION:-a88d1f687ec83ff82b5318f59dcecb8dab44fc83}
CC="$WASI_SDK_PATH/bin/clang --target=wasm32-wasip3 -O2"
mkdir -p "$OUT"

if [ -z "$WIT_BRIDGE_GEN" ]; then
  WIT_BRIDGE_GEN="$OUT/wit-bridge-gen"
  (cd "$ROOT" && go build -o "$WIT_BRIDGE_GEN" ./cmd/wit-bridge-gen)
fi
if [ -z "${JMP_SRC:-}" ]; then
  JMP_SRC="$OUT/bochs-jmp"
  if [ ! -d "$JMP_SRC" ]; then
    git clone --filter=blob:none --no-checkout "$BOCHS_REPO" "$OUT/bochs-src"
    (cd "$OUT/bochs-src" && git checkout "$BOCHS_REPO_VERSION" -- bochs/wasi_extra/jmp)
    cp -r "$OUT/bochs-src/bochs/wasi_extra/jmp" "$JMP_SRC"
  fi
fi

# 1. Bridge component for the test world.
BRIDGE="$OUT/bridge"
rm -rf "$BRIDGE" && mkdir -p "$BRIDGE/wit/deps/app" "$BRIDGE/wit/deps/c2w-bridge" "$BRIDGE/wit/deps/wasi-cli-run" "$BRIDGE/src"
wasm-tools component wit --json wit > "$OUT/resolve.json"
"$WIT_BRIDGE_GEN" --resolve "$OUT/resolve.json" --out "$OUT/gen"
cp wit/*.wit "$BRIDGE/wit/deps/app/"
cp "$ROOT/extras/wasip3/wit/c2w-bridge/"*.wit "$BRIDGE/wit/deps/c2w-bridge/"
cp "$ROOT/extras/wasip3/bridge/wit/deps/wasi-cli-run/"*.wit "$BRIDGE/wit/deps/wasi-cli-run/"
cp "$OUT/gen/world.wit" "$BRIDGE/wit/"
cp "$ROOT/extras/wasip3/bridge/Cargo.toml" "$ROOT/extras/wasip3/bridge/src/lib.rs" "$BRIDGE/" 2>/dev/null || true
cp "$ROOT/extras/wasip3/bridge/src/lib.rs" "$BRIDGE/src/"
[ -f "$ROOT/extras/wasip3/bridge/Cargo.lock" ] && cp "$ROOT/extras/wasip3/bridge/Cargo.lock" "$BRIDGE/"
cp "$OUT/gen/generated.rs" "$BRIDGE/src/"
(cd "$BRIDGE" && cargo build --release --target wasm32-unknown-unknown)
wasm-tools component new "$BRIDGE/target/wasm32-unknown-unknown/release/c2w_bridge.wasm" -o "$OUT/bridge.wasm"

# 2. Fake VM core module.
VM="$OUT/vm"
rm -rf "$VM" && mkdir -p "$VM/pack"
echo "fake pack content" > "$VM/pack/hello.txt"
wit-bindgen c --out-dir "$VM/gen" --world vm-core "$ROOT/extras/wasip3/wit/c2w-bridge"
"$ROOT/extras/wasip3/scripts/gen-pack-list.sh" "$VM/pack" > "$VM/c2w_pack_list.h"
SRC="$ROOT/extras/wasip3/vmcore"
CFLAGS="-I$SRC -I$VM/gen -I$JMP_SRC -I$VM -DC2W_BRIDGE_DESCRIPTION_PATH=\"$OUT/gen/description.json\""
for f in c2w_vmcore c2w_jmp_p3 c2w_pack; do
  $CC $CFLAGS -c "$SRC/$f.c" -o "$VM/$f.o"
done
$CC $CFLAGS -c fakevm.c -o "$VM/fakevm.o"
$CC $CFLAGS -c "$VM/gen/vm_core.c" -o "$VM/vm_core.o"
$CC $CFLAGS -c "$JMP_SRC/jmp.c" -o "$VM/jmp.o"
$CC -mexec-model=reactor -fuse-ld=lld \
  -Wl,--wrap=open,--wrap=__wasilibc_open_nomode,--wrap=read,--wrap=readv,--wrap=pread,--wrap=lseek,--wrap=close,--wrap=fstat,--wrap=stat,--wrap=access \
  -Wl,--export=wasm_setjmp,--export=wasm_longjmp,--export=handle_jmp,--export=wasm_setjmp_internal \
  -o "$VM/fakevm.core.wasm" "$VM"/*.o "$VM/gen/vm_core_component_type.o"
"$BINARYEN_PATH/wasm-opt" "$VM/fakevm.core.wasm" --asyncify -O2 --pass-arg=asyncify-ignore-imports -o "$VM/fakevm.async.wasm"
wasm-tools component new "$VM/fakevm.async.wasm" -o "$OUT/fakevm.wasm"

# 3. Compose.
wac plug "$OUT/bridge.wasm" --plug "$OUT/fakevm.wasm" -o "$OUT/composed.wasm"
wasm-tools component wit "$OUT/composed.wasm" | sed -n '/^world root/,/^}/p'
echo "built $OUT/composed.wasm"
