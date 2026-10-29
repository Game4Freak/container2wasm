#!/bin/bash
# Build Bochs as a WASI 0.3 core module and turn it into a component.
#
# usage: build-bochs-p3.sh [compile|link|all]
#
#   compile  configure and compile Bochs and the VM core runtime (no pack
#            needed); the result stays in BOCHS_SRC and OUT.
#   link     embed the pack files and the WIT description, link, asyncify
#            and componentize (OUT/bochs.wasm). Requires a previous compile
#            in the same trees.
#   all      both (default).
#
# Environment:
#   WASI_SDK_PATH     wasi-sdk >= 34
#   BINARYEN_PATH     directory containing wasm-opt
#   BOCHS_SRC         Bochs tree (ktock/Bochs fork) with bochs-wasip3.patch applied
#   PACK_DIR          (link) files to embed under /pack (bochsrc, BIOS images, boot.iso, rootfs.bin)
#   DESCRIPTION_JSON  (link) WIT description produced by build-bridge.sh
#   OUT               output directory (bochs.wasm)
#   C2W_SRC           repository root (default: derived from this script)
#   INIT_DEBUG        true to enable Bochs logging (default: false)
#   JOBS              parallelism for make (default: nproc)
set -euo pipefail
STEP=${1:-all}
: "${WASI_SDK_PATH:?}" "${BINARYEN_PATH:?}" "${BOCHS_SRC:?}" "${OUT:?}"
C2W_SRC=${C2W_SRC:-$(cd "$(dirname "$0")/../../.." && pwd)}
INIT_DEBUG=${INIT_DEBUG:-false}
JOBS=${JOBS:-$(nproc)}
P3="$C2W_SRC/extras/wasip3"
VMCORE="$P3/vmcore"
mkdir -p "$OUT/gen" "$OUT/jmp" "$OUT/obj"
OUT=$(cd "$OUT" && pwd)

# Bochs' sub-makefiles call `ar` and `ranlib` from PATH: use wasi-sdk's.
export PATH="$WASI_SDK_PATH/bin:$PATH"
CC="$WASI_SDK_PATH/bin/clang"
CXX="$WASI_SDK_PATH/bin/clang++"
TARGET="--target=wasm32-wasip3"
COMMON_FLAGS="$TARGET -D_WASI_EMULATED_SIGNAL -DWASI -DWASIP3 -D__GNU__ -O2 -I$OUT/jmp -I$OUT/gen -I$VMCORE"
# The link flags are passed to make only: with them, configure's link tests
# would succeed for every function (reactor modules need no main) and
# Bochs would use functions wasi-libc does not provide (mkstemp, stricmp).
LDFLAGS="-mexec-model=reactor -fuse-ld=lld -Wl,--wrap=open,--wrap=__wasilibc_open_nomode,--wrap=read,--wrap=readv,--wrap=pread,--wrap=lseek,--wrap=close,--wrap=fstat,--wrap=stat,--wrap=lstat,--wrap=access,--wrap=exit,--wrap=wasm_longjmp"
# Objects linked into Bochs besides its own (the pack object is added by link).
EMU_OBJS="$OUT/jmp/jmp $OUT/obj/c2w_vmcore.o $OUT/obj/vm_core.o $OUT/obj/wasm_bridge.o $OUT/gen/vm_core_component_type.o"

# pack_object <pack dir> <description.json>: compile c2w_pack.o embedding them.
pack_object() {
  "$P3/scripts/gen-pack-list.sh" "$1" > "$OUT/c2w_pack_list.h"
  $CC $COMMON_FLAGS -I"$OUT" -DC2W_BRIDGE_DESCRIPTION_PATH="\"$2\"" -c "$VMCORE/c2w_pack.c" -o "$OUT/obj/c2w_pack.o"
}

compile() {
  # 1. Bindings of the c2w:bridge/vm interface.
  wit-bindgen c --out-dir "$OUT/gen" --world vm-core "$P3/wit/c2w-bridge"

  # 2. Asyncify based setjmp/longjmp (jmp.c of the fork + the p3 stack pointer wrapper).
  JMP="$BOCHS_SRC/bochs/wasi_extra/jmp"
  cp "$JMP/jmp.h" "$OUT/jmp/"
  $CC $TARGET -O2 -c "$JMP/jmp.c" -I "$JMP" -o "$OUT/jmp/jmp.o"
  $CC $TARGET -O2 -c "$VMCORE/c2w_jmp_p3.c" -I "$JMP" -o "$OUT/jmp/jmp_p3.o"
  "$WASI_SDK_PATH/bin/wasm-ld" "$OUT/jmp/jmp.o" "$OUT/jmp/jmp_p3.o" \
    --export=wasm_setjmp --export=wasm_longjmp --export=handle_jmp --no-entry -r -o "$OUT/jmp/jmp"

  # 3. VM core runtime.
  $CC $COMMON_FLAGS -c "$VMCORE/c2w_vmcore.c" -o "$OUT/obj/c2w_vmcore.o"
  $CC $COMMON_FLAGS -c "$OUT/gen/vm_core.c" -o "$OUT/obj/vm_core.o"

  # 4. Bochs.
  LOGGING_FLAG=--disable-logging
  if [ "$INIT_DEBUG" = "true" ]; then LOGGING_FLAG=--enable-logging; fi
  cd "$BOCHS_SRC/bochs"
  # LD is only used by configure's libtool checks (wasi-sdk ships no `ld`).
  CC="$CC" CXX="$CXX" RANLIB="$WASI_SDK_PATH/bin/ranlib" AR="$WASI_SDK_PATH/bin/ar" LD="$WASI_SDK_PATH/bin/wasm-ld" \
    CFLAGS="$COMMON_FLAGS" CXXFLAGS="$COMMON_FLAGS" \
    ./configure --host wasm32-unknown-wasi --enable-x86-64 --with-nogui --enable-usb --enable-usb-ehci \
    --disable-large-ramfile --disable-show-ips --disable-stats $LOGGING_FLAG \
    --enable-repeat-speedups --enable-fast-function-calls --disable-trace-linking --enable-handlers-chaining --enable-avx
  $CXX $COMMON_FLAGS -I. -I./instrument/stubs -c "$P3/bochs/wasm_bridge.cc" -o "$OUT/obj/wasm_bridge.o"
  # Compile and link once with a placeholder pack so that errors surface
  # here; `link` only rebuilds the pack object and relinks.
  mkdir -p "$OUT/placeholder-pack"
  echo placeholder > "$OUT/placeholder-pack/placeholder"
  echo '{}' > "$OUT/placeholder-description.json"
  pack_object "$OUT/placeholder-pack" "$OUT/placeholder-description.json"
  make -j"$JOBS" bochs LDFLAGS="$LDFLAGS" EMU_DEPS="$EMU_OBJS $OUT/obj/c2w_pack.o -lrt"
  rm -f bochs
  echo "compiled Bochs in $BOCHS_SRC"
}

link() {
  : "${PACK_DIR:?}" "${DESCRIPTION_JSON:?}"
  PACK_DIR=$(cd "$PACK_DIR" && pwd)
  DESCRIPTION_JSON=$(cd "$(dirname "$DESCRIPTION_JSON")" && pwd)/$(basename "$DESCRIPTION_JSON")

  # 5. Embedded pack files and WIT description.
  pack_object "$PACK_DIR" "$DESCRIPTION_JSON"

  # 6. Link.
  cd "$BOCHS_SRC/bochs"
  rm -f bochs
  make -j"$JOBS" bochs LDFLAGS="$LDFLAGS" EMU_DEPS="$EMU_OBJS $OUT/obj/c2w_pack.o -lrt"
  mv bochs "$OUT/bochs.core.wasm"

  # 7. Asyncify (setjmp/longjmp and cooperative yields) and componentize.
  # -g keeps the name section so that traps come with readable backtraces;
  # the DWARF sections (from wasi-libc) are stale after optimization and
  # would mislead symbolizers, so they are dropped.
  "$BINARYEN_PATH/wasm-opt" "$OUT/bochs.core.wasm" --asyncify -O2 -g --strip-dwarf --pass-arg=asyncify-ignore-imports -o "$OUT/bochs.async.wasm"
  wasm-tools component new "$OUT/bochs.async.wasm" -o "$OUT/bochs.wasm"
  rm -f "$OUT/bochs.core.wasm" "$OUT/bochs.async.wasm"
  echo "built $OUT/bochs.wasm"
}

case "$STEP" in
  compile) compile ;;
  link) link ;;
  all) compile; link ;;
  *) echo "usage: $0 [compile|link|all]" >&2; exit 2 ;;
esac
