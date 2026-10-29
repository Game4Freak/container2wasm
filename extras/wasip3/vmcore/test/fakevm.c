/*
 * A stand-in for the emulator used to test the VM core runtime without
 * booting Linux. It plays both roles of the real system:
 *
 *  - the emulator main loop, which regularly yields with c2w_vm_yield();
 *  - the guest bridge daemon, which talks the frame protocol: it sends
 *    HELLO, issues CALL frames for the `c2w:test/host` functions and answers
 *    INVOKE frames for the `c2w:test/guest` functions.
 *
 * Progress is reported on stdout so that the host test can check it.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "c2w_bridge_proto.h"
#include "c2w_vmcore.h"

#define MAX_ITER 2000000

static uint8_t rx[1 << 16];
static size_t rx_len;
static int results_seen;
static int invokes_seen;
static uint32_t next_call_id = 1;

/* "guest -> emulator" direction: the daemon writes a frame on the port. */
static void guest_send(uint8_t type, uint32_t id, const char *payload) {
  size_t len = payload ? strlen(payload) : 0;
  uint8_t *buf = malloc(C2W_FRAME_HEADER_SIZE + len);
  c2w_frame_header_t h = {C2W_BRIDGE_MAGIC, C2W_BRIDGE_VERSION, type, 0, id, (uint32_t)len};
  memcpy(buf, &h, sizeof(h));
  if (len)
    memcpy(buf + C2W_FRAME_HEADER_SIZE, payload, len);
  /* Deliver in two chunks to exercise the stream reassembly. */
  size_t first = (C2W_FRAME_HEADER_SIZE + len) / 2;
  c2w_bridge_rx(buf, first);
  c2w_bridge_rx(buf + first, C2W_FRAME_HEADER_SIZE + len - first);
  free(buf);
}

static void guest_call(const char *iface, const char *func, const char *args) {
  char doc[512];
  snprintf(doc, sizeof(doc), "{\"interface\":\"%s\",\"function\":\"%s\",\"args\":%s}", iface, func,
           args);
  printf("guest: call %u %s.%s %s\n", next_call_id, iface, func, args);
  guest_send(C2W_FRAME_CALL, next_call_id++, doc);
}

/* Extract the "args" array of an INVOKE payload (without the brackets). */
static void extract_args(const char *payload, size_t len, char *out, size_t outsz) {
  const char *p = memmem(payload, len, "\"args\":[", 8);
  if (p == NULL) {
    out[0] = 0;
    return;
  }
  p += 8;
  const char *end = payload + len;
  /* the payload ends with "]}" */
  while (end > p && *(end - 1) != ']')
    end--;
  size_t n = (size_t)(end - 1 - p);
  if (n >= outsz)
    n = outsz - 1;
  memcpy(out, p, n);
  out[n] = 0;
}

static void guest_handle_invoke(uint32_t id, const char *payload, size_t len) {
  char args[1024];
  extract_args(payload, len, args, sizeof(args));
  char resp[1200];
  invokes_seen++;
  if (memmem(payload, len, "\"function\":\"echo\"", 17) != NULL) {
    /* args is a single JSON string: echo it back unchanged. */
    snprintf(resp, sizeof(resp), "{\"result\":%s}", args);
  } else if (memmem(payload, len, "\"function\":\"sum-list\"", 21) != NULL) {
    /* args is "[1,2,3]": sum the numbers. */
    unsigned long sum = 0, cur = 0;
    int in_num = 0;
    for (const char *c = args; *c; c++) {
      if (*c >= '0' && *c <= '9') {
        cur = cur * 10 + (unsigned long)(*c - '0');
        in_num = 1;
      } else if (in_num) {
        sum += cur;
        cur = 0;
        in_num = 0;
      }
    }
    if (in_num)
      sum += cur;
    snprintf(resp, sizeof(resp), "{\"result\":%lu}", sum);
  } else {
    snprintf(resp, sizeof(resp), "{\"error\":\"no handler\"}");
  }
  printf("guest: invoke %u -> %s\n", id, resp);
  fflush(stdout);
  guest_send(C2W_FRAME_RETURN, id, resp);
}

/* "emulator -> guest" direction: drain the frames the daemon would read. */
static void guest_poll(void) {
  rx_len += c2w_bridge_tx_pull(rx + rx_len, sizeof(rx) - rx_len);
  size_t off = 0;
  while (rx_len - off >= C2W_FRAME_HEADER_SIZE) {
    c2w_frame_header_t h;
    memcpy(&h, rx + off, sizeof(h));
    if (rx_len - off < C2W_FRAME_HEADER_SIZE + h.len)
      break;
    const char *payload = (const char *)rx + off + C2W_FRAME_HEADER_SIZE;
    switch (h.type) {
    case C2W_FRAME_DESCRIBE:
      printf("guest: describe (%u bytes) %s\n", h.len,
             memmem(payload, h.len, "\"worlds\"", 8) != NULL ? "ok" : "unexpected");
      guest_call("c2w:test/host@0.1.0", "add", "[2,3]");
      guest_call("c2w:test/host@0.1.0", "greet", "[\"vm\"]");
      guest_call("c2w:test/host@0.1.0", "swap", "[{\"x\":1,\"y\":2}]");
      guest_call("c2w:test/host@0.1.0", "missing", "[]");
      break;
    case C2W_FRAME_RESULT:
      results_seen++;
      printf("guest: result %u %.*s\n", h.id, (int)h.len, payload);
      break;
    case C2W_FRAME_INVOKE:
      guest_handle_invoke(h.id, payload, h.len);
      break;
    default:
      printf("guest: unexpected frame type %u\n", h.type);
      break;
    }
    fflush(stdout);
    off += C2W_FRAME_HEADER_SIZE + h.len;
  }
  memmove(rx, rx + off, rx_len - off);
  rx_len -= off;
}

int c2w_vm_entry(void) {
  printf("fakevm: boot\n");
  fflush(stdout);
  guest_send(C2W_FRAME_HELLO, 0, NULL);
  for (long i = 0; i < MAX_ITER; i++) {
    guest_poll();
    if (results_seen >= 4 && invokes_seen >= 2) {
      printf("fakevm: done after %ld iterations\n", i);
      fflush(stdout);
      return 0;
    }
    c2w_vm_yield();
  }
  printf("fakevm: timeout (results=%d invokes=%d)\n", results_seen, invokes_seen);
  return 1;
}
