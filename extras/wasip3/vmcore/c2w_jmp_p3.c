/*
 * WASI 0.3 replacement for wasi_extra/jmp/jmp.S of the Bochs fork.
 *
 * With the cooperative-threads ABI used by wasm32-wasip3 the shadow stack
 * pointer is not a wasm global but lives in the task's context slot and is
 * accessed through the `__wasm_{get,set}_stack_pointer` libcalls. The
 * wrapper below saves it in a local (which Asyncify captures) around the
 * setjmp unwind/rewind so that it is restored after a longjmp.
 */
#include <stdint.h>

#include "jmp.h"

extern uint32_t __wasm_get_stack_pointer(void);
extern void __wasm_set_stack_pointer(uint32_t);
int wasm_setjmp_internal(jmp_buf *buf);

__attribute__((noinline, export_name("wasm_setjmp")))
int wasm_setjmp(jmp_buf *buf) {
  uint32_t sp = __wasm_get_stack_pointer(); /* kept in a wasm local, saved by Asyncify */
  int ret = wasm_setjmp_internal(buf);
  __wasm_set_stack_pointer(sp);
  return ret;
}
