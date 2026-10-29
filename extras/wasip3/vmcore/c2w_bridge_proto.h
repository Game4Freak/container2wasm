/*
 * Frame protocol spoken between the guest bridge daemon (cmd/init) and the
 * emulator over the dedicated virtio-console port (/dev/hvc1 in the guest).
 *
 * Every frame starts with a fixed 16 byte little-endian header followed by
 * `len` payload bytes. The Go side mirrors these constants in
 * cmd/init/bridge.go. See docs/wasip3-bridge-protocol.md.
 */
#ifndef C2W_BRIDGE_PROTO_H
#define C2W_BRIDGE_PROTO_H

#include <stdint.h>

#define C2W_BRIDGE_MAGIC   0x42573243u /* "C2WB" */
#define C2W_BRIDGE_VERSION 1

/* Guest -> emulator: HELLO, payload unused. Answered with DESCRIBE. */
#define C2W_FRAME_HELLO    1
/* Emulator -> guest: payload is the JSON description of the WIT world. */
#define C2W_FRAME_DESCRIBE 2
/* Guest -> emulator: call a host function. id chosen by the guest,
 * payload is {"interface":..,"function":..,"args":[..]}. */
#define C2W_FRAME_CALL     3
/* Emulator -> guest: response for CALL with the same id,
 * payload is {"result":..} or {"error":".."}. */
#define C2W_FRAME_RESULT   4
/* Emulator -> guest: invoke a function implemented in the guest. id chosen
 * by the emulator, payload is {"interface":..,"function":..,"args":[..]}. */
#define C2W_FRAME_INVOKE   5
/* Guest -> emulator: response for INVOKE with the same id,
 * payload is {"result":..} or {"error":".."}. */
#define C2W_FRAME_RETURN   6

#define C2W_FRAME_HEADER_SIZE 16
#define C2W_FRAME_MAX_PAYLOAD (64u * 1024u * 1024u)

typedef struct c2w_frame_header {
  uint32_t magic;
  uint8_t version;
  uint8_t type;
  uint16_t reserved;
  uint32_t id;
  uint32_t len;
} c2w_frame_header_t;

#endif
