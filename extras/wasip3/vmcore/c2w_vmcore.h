/*
 * Runtime glue implementing the `c2w:bridge/vm` interface for an emulator
 * compiled to a WASI 0.3 component.
 *
 * The emulator provides the three functions below; everything else
 * (component model async exports, futures, frame parsing) lives in
 * c2w_vmcore.c.
 */
#ifndef C2W_VMCORE_H
#define C2W_VMCORE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ---- implemented by the emulator ------------------------------------- */

/* Entry point of the emulator. Called once from the `run` task; returns the
 * exit code when the virtual machine has shut down. */
int c2w_vm_entry(void);

/* ---- provided by c2w_vmcore.c ---------------------------------------- */

/* Must be called regularly from the emulator main loop (any stack depth).
 * Unwinds the stack with Asyncify, hands control back to the component
 * runtime so that other tasks can run, and returns once the runtime resumes
 * the `run` task. */
void c2w_vm_yield(void);

/* Bytes written by the guest on the bridge port. */
void c2w_bridge_rx(const uint8_t *buf, size_t len);

/* Pull up to `len` bytes destined to the guest. Returns the number of bytes
 * copied (0 if nothing is pending). */
size_t c2w_bridge_tx_pull(uint8_t *buf, size_t len);

/* True when c2w_bridge_tx_pull would return data. */
bool c2w_bridge_tx_pending(void);

/* True when a verbose log of bridge activity should be printed to stderr
 * (controlled by the C2W_BRIDGE_DEBUG environment variable). */
bool c2w_bridge_debug(void);

#ifdef __cplusplus
}
#endif

#endif
