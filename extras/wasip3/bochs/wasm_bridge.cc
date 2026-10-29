/////////////////////////////////////////////////////////////////////////
//
//  container2wasm WASI 0.3 bridge device for Bochs.
//
//  A second virtio-console (the guest sees it as /dev/hvc1) carries the
//  frame protocol between the bridge daemon of the guest (cmd/init) and
//  the VM core runtime (extras/wasip3/vmcore). The device timer also drives
//  the cooperative yield of the `run` task.
//
//  Compiled outside of the Bochs tree with the same flags as wasm.cc; see
//  extras/wasip3/scripts/build-bochs-p3.sh.
//
/////////////////////////////////////////////////////////////////////////

#include "bochs.h"
#include "iodev/iodev.h"

#if BX_SUPPORT_PCI

#include "iodev/pci.h"
#include "wasm.h"

extern "C" {
#include "c2w_vmcore.h"
int __main_void(void);
}

#define LOG_THIS genlog->

// Defined in wasm.cc.
Bit16u virtio_read16(virtio_phys_addr_t addr);

#ifndef C2W_BRIDGE_YIELD_USEC
/* Emulated time between two yields of the run task. */
#define C2W_BRIDGE_YIELD_USEC 1000
#endif

class bx_virtio_bridge_ctrl_c : public bx_virtio_ctrl_c {
public:
  bx_virtio_bridge_ctrl_c(char *plugin_name);
  virtual ~bx_virtio_bridge_ctrl_c();
  virtual void init(void);
  virtual int device_recv(int queue_idx, int desc_idx, int read_size, int write_size);
  static void timer_handler(void *this_ptr);

private:
  bool can_write_data();
  int get_write_len();
  int write_data(const uint8_t *buf, int buf_len);
  void flush_tx();

  char *plugin_name;
  int timer_id;
};

bx_virtio_bridge_ctrl_c::bx_virtio_bridge_ctrl_c(char *plugin_name)
{
  put("VIRTIO BRIDGE");
  this->plugin_name = plugin_name;
}

bx_virtio_bridge_ctrl_c::~bx_virtio_bridge_ctrl_c()
{
  SIM->get_bochs_root()->remove("virtio_bridge");
  BX_DEBUG(("Exit"));
}

void bx_virtio_bridge_ctrl_c::init()
{
  bx_virtio_ctrl_c::init(plugin_name, 0x1003, 0x0780, 3, 1 << 0, "Virtio-console (c2w bridge)");
  config_space_size = 4;
  s.queue[0].manual_recv = true;
  timer_id = DEV_register_timer(this, timer_handler, C2W_BRIDGE_YIELD_USEC, true, true, "virtio-bridge");
}

// Queue 1 carries data written by the guest.
int bx_virtio_bridge_ctrl_c::device_recv(int queue_idx, int desc_idx, int read_size, int write_size)
{
  if (queue_idx == 1) {
    uint8_t *buf = (uint8_t *)malloc(read_size);
    memcpy_from_queue(buf, queue_idx, desc_idx, 0, read_size);
    c2w_bridge_rx(buf, read_size);
    free(buf);
    virtio_consume_desc(queue_idx, desc_idx, 0);
  }
  return 0;
}

bool bx_virtio_bridge_ctrl_c::can_write_data()
{
  QueueState *qs = &s.queue[0];
  if (!qs->ready)
    return false;
  return qs->last_avail_idx != virtio_read16(qs->avail_addr + 2);
}

int bx_virtio_bridge_ctrl_c::get_write_len()
{
  QueueState *qs = &s.queue[0];
  int desc_idx, read_size, write_size;
  if (!qs->ready)
    return 0;
  if (qs->last_avail_idx == virtio_read16(qs->avail_addr + 2))
    return 0;
  desc_idx = virtio_read16(qs->avail_addr + 4 + (qs->last_avail_idx & (qs->num - 1)) * 2);
  if (get_desc_rw_size(&read_size, &write_size, 0, desc_idx))
    return 0;
  return write_size;
}

int bx_virtio_bridge_ctrl_c::write_data(const uint8_t *buf, int buf_len)
{
  QueueState *qs = &s.queue[0];
  int desc_idx;
  if (!qs->ready)
    return 0;
  if (qs->last_avail_idx == virtio_read16(qs->avail_addr + 2))
    return 0;
  desc_idx = virtio_read16(qs->avail_addr + 4 + (qs->last_avail_idx & (qs->num - 1)) * 2);
  memcpy_to_queue(0, desc_idx, 0, (void *)buf, buf_len);
  virtio_consume_desc(0, desc_idx, buf_len);
  qs->last_avail_idx++;
  return buf_len;
}

// Hand pending frames to the guest as long as it offers receive buffers.
void bx_virtio_bridge_ctrl_c::flush_tx()
{
  uint8_t buf[4096];
  while (c2w_bridge_tx_pending() && can_write_data()) {
    int len = get_write_len();
    if (len <= 0)
      break;
    if (len > (int)sizeof(buf))
      len = sizeof(buf);
    size_t n = c2w_bridge_tx_pull(buf, (size_t)len);
    if (n == 0)
      break;
    write_data(buf, (int)n);
  }
}

void bx_virtio_bridge_ctrl_c::timer_handler(void *this_ptr)
{
  bx_virtio_bridge_ctrl_c *dev = (bx_virtio_bridge_ctrl_c *)this_ptr;
  dev->flush_tx();
  // Give the component runtime a chance to run the bridge tasks.
  c2w_vm_yield();
  dev->flush_tx();
}

static bx_virtio_bridge_ctrl_c *bridge_v;

PLUGIN_ENTRY_FOR_MODULE(bridgeVirtioConsole)
{
  if (mode == PLUGIN_INIT) {
    bridge_v = new bx_virtio_bridge_ctrl_c(BX_PLUGIN_BRIDGE_VIRTIO_CONSOLE);
    BX_REGISTER_DEVICE_DEVMODEL(plugin, type, bridge_v, BX_PLUGIN_BRIDGE_VIRTIO_CONSOLE);
  } else if (mode == PLUGIN_FINI) {
    delete bridge_v;
  } else if (mode == PLUGIN_PROBE) {
    return (int)PLUGTYPE_STANDARD;
  } else if (mode == PLUGIN_FLAGS) {
    return PLUGFLAG_PCI;
  }
  return 0;
}

// Entry point of the emulator for the `run` task (see c2w_vmcore.c).
extern "C" int c2w_vm_entry(void)
{
  return __main_void();
}

#endif /* BX_SUPPORT_PCI */
