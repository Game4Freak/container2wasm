#!/bin/bash
# Build Bochs as a WASI 0.3 core module and turn it into a component.
#
# usage: build-bochs-p3.sh
#
# Environment:
#   WASI_SDK_PATH     wasi-sdk >= 34
#   BINARYEN_PATH     directory containing wasm-opt
#   BOCHS_SRC         Bochs tree (ktock/Bochs fork) with bochs-wasip3.patch applied
#   PACK_DIR          files to embed under /pack (bochsrc, BIOS images, boot.iso, rootfs.bin)
#   DESCRIPTION_JSON  WIT description produced by build-bridge.sh
#   OUT               output directory (bochs.wasm)
#   C2W_SRC           repository root (default: derived from this script)
#   INIT_DEBUG        true to enable Bochs logging (default: false)
#   JOBS              parallelism for make (default: nproc)
set -euo pipefail
: "${WASI_SDK_PATH:?}" "${BINARYEN_PATH:?}" "${BOCHS_SRC:?}" "${PACK_DIR:?}" "${DESCRIPTION_JSON:?}" "${OUT:?}"
C2W_SRC=${C2W_SRC:-$(cd "$(dirname "$0")/../../.." && pwd)}
INIT_DEBUG=${INIT_DEBUG:-false}
JOBS=${JOBS:-$(nproc)}
P3="$C2W_SRC/extras/wasip3"
VMCORE="$P3/vmcore"
mkdir -p "$OUT/gen" "$OUT/jmp" "$OUT/obj"
OUT=$(cd "$OUT" && pwd)
PACK_DIR=$(cd "$PACK_DIR" && pwd)
DESCRIPTION_JSON=$(cd "$(dirname "$DESCRIPTION_JSON")" && pwd)/$(basename "$DESCRIPTION_JSON")

CC="$WASI_SDK_PATH/bin/clang"
CXX="$WASI_SDK_PATH/bin/clang++"
TARGET="--target=wasm32-wasip3"
COMMON_FLAGS="$TARGET -D_WASI_EMULATED_SIGNAL -DWASI -DWASIP3 -D__GNU__ -O2 -I$OUT/jmp -I$OUT/gen -I$VMCORE"

# 1. Bindings of the c2w:bridge/vm interface.
wit-bindgen c --out-dir "$OUT/gen" --world vm-core "$P3/wit/c2w-bridge"

# 2. Asyncify based setjmp/longjmp (jmp.c of the fork + the p3 stack pointer wrapper).
JMP="$BOCHS_SRC/bochs/wasi_extra/jmp"
cp "$JMP/jmp.h" "$OUT/jmp/"
$CC $TARGET -O2 -c "$JMP/jmp.c" -I "$JMP" -o "$OUT/jmp/jmp.o"
$CC $TARGET -O2 -c "$VMCORE/c2w_jmp_p3.c" -I "$JMP" -o "$OUT/jmp/jmp_p3.o"
"$WASI_SDK_PATH/bin/wasm-ld" "$OUT/jmp/jmp.o" "$OUT/jmp/jmp_p3.o" \
  --export=wasm_setjmp --export=wasm_longjmp --export=handle_jmp --no-entry -r -o "$OUT/jmp/jmp"

# 3. VM core runtime and embedded pack files.
"$P3/scripts/gen-pack-list.sh" "$PACK_DIR" > "$OUT/c2w_pack_list.h"
$CC $COMMON_FLAGS -I"$OUT" -DC2W_BRIDGE_DESCRIPTION_PATH="\"$DESCRIPTION_JSON\"" -c "$VMCORE/c2w_pack.c" -o "$OUT/obj/c2w_pack.o"
$CC $COMMON_FLAGS -c "$VMCORE/c2w_vmcore.c" -o "$OUT/obj/c2w_vmcore.o"
$CC $COMMON_FLAGS -c "$OUT/gen/vm_core.c" -o "$OUT/obj/vm_core.o"

# 4. Bochs.
LOGGING_FLAG=--disable-logging
if [ "$INIT_DEBUG" = "true" ]; then LOGGING_FLAG=--enable-logging; fi
# The link flags are passed to make only: with them, configure's link tests
# would succeed for every function (reactor modules need no main) and
# Bochs would use functions wasi-libc does not provide (mkstemp, stricmp).
LDFLAGS="-mexec-model=reactor -fuse-ld=lld -Wl,--wrap=open,--wrap=__wasilibc_open_nomode,--wrap=read,--wrap=readv,--wrap=pread,--wrap=lseek,--wrap=close,--wrap=fstat,--wrap=stat,--wrap=access"
cd "$BOCHS_SRC/bochs"
CC="$CC" CXX="$CXX" RANLIB="$WASI_SDK_PATH/bin/ranlib" AR="$WASI_SDK_PATH/bin/ar" \
  CFLAGS="$COMMON_FLAGS" CXXFLAGS="$COMMON_FLAGS" \
  ./configure --host wasm32-unknown-wasi --enable-x86-64 --with-nogui --enable-usb --enable-usb-ehci \
  --disable-large-ramfile --disable-show-ips --disable-stats $LOGGING_FLAG \
  --enable-repeat-speedups --enable-fast-function-calls --disable-trace-linking --enable-handlers-chaining --enable-avx
$CXX $COMMON_FLAGS -I. -I./instrument/stubs -c "$P3/bochs/wasm_bridge.cc" -o "$OUT/obj/wasm_bridge.o"
make -j"$JOBS" bochs LDFLAGS="$LDFLAGS" EMU_DEPS="$OUT/jmp/jmp $OUT/obj/c2w_pack.o $OUT/obj/c2w_vmcore.o $OUT/obj/vm_core.o $OUT/obj/wasm_bridge.o $OUT/gen/vm_core_component_type.o -lrt"
mv bochs "$OUT/bochs.core.wasm"

# 5. Asyncify (setjmp/longjmp and cooperative yields) and componentize.
"$BINARYEN_PATH/wasm-opt" "$OUT/bochs.core.wasm" --asyncify -O2 --pass-arg=asyncify-ignore-imports -o "$OUT/bochs.async.wasm"
wasm-tools component new "$OUT/bochs.async.wasm" -o "$OUT/bochs.wasm"
rm -f "$OUT/bochs.core.wasm" "$OUT/bochs.async.wasm"
echo "built $OUT/bochs.wasm"
