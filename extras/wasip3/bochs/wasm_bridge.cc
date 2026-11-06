/////////////////////////////////////////////////////////////////////////
//
//  container2wasm WASI 0.3 bridge device for Bochs.
//
//  A second virtio-console (the guest sees it as /dev/hvc1) carries the
//  frame protocol between the bridge daemon of the guest (cmd/init) and
//  the VM core runtime (extras/wasip3/vmcore). The device timer also drives
//  the cooperative yield of the `run` task.
//
//  The file also provides the backend of the virtio-net device for WASI
//  0.3: ethernet frames are handed to the VM core runtime, which exchanges
//  them with the network stack running in the bridge component.
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

/////////////////////////////////////////////////////////////////////////
// virtio-net backend
/////////////////////////////////////////////////////////////////////////

// Defined in wasm.cc.
extern bx_virtio_net_ctrl_c *socket0;
extern EthernetDevice *socket_net;

static void net_write_packet(EthernetDevice *net, const uint8_t *buf, int len)
{
  c2w_net_rx(buf, (size_t)len);
}

static uint8_t *net_alloc_packet(EthernetDevice *net, int len)
{
  return (uint8_t *)malloc(len > 0 ? len : 1);
}

static void net_select_fill(EthernetDevice *net, int *pfd_max, fd_set *rfds, fd_set *wfds, fd_set *efds, int *pdelay)
{
}

static void net_select_poll(EthernetDevice *net, fd_set *rfds, fd_set *wfds, fd_set *efds, int select_ret)
{
}

// Inbound frames are delivered from the bridge timer (net_flush_rx), so the
// receive timer of the device has nothing to do.
static int net_watch(EthernetDevice *net)
{
  return -1;
}

EthernetDevice *c2w_net_device_init(void)
{
  EthernetDevice *net = (EthernetDevice *)calloc(1, sizeof(*net));
  net->mac_addr[0] = 0x02;
  net->mac_addr[1] = 0x00;
  net->mac_addr[2] = 0x00;
  net->mac_addr[3] = 0x00;
  net->mac_addr[4] = 0x00;
  net->mac_addr[5] = 0x01;
  net->opaque = NULL;
  net->alloc_packet = net_alloc_packet;
  net->write_packet = net_write_packet;
  net->select_fill = net_select_fill;
  net->select_poll = net_select_poll;
  net->watch = net_watch;
  return net;
}

// Hand the frames queued by the bridge component to the guest as long as it
// offers receive buffers.
static void net_flush_rx()
{
  static uint8_t *buf;
  const size_t cap = 65536;
  if (socket0 == NULL || socket_net == NULL)
    return;
  if (buf == NULL)
    buf = (uint8_t *)malloc(cap);
  while (c2w_net_tx_pending() && socket0->virtio_net_can_write_packet(socket_net)) {
    size_t n = c2w_net_tx_pull(buf, cap);
    if (n > 0)
      socket0->virtio_net_write_packet(socket_net, buf, (int)n);
  }
}

void bx_virtio_bridge_ctrl_c::timer_handler(void *this_ptr)
{
  bx_virtio_bridge_ctrl_c *dev = (bx_virtio_bridge_ctrl_c *)this_ptr;
  dev->flush_tx();
  net_flush_rx();
  // Give the component runtime a chance to run the bridge tasks.
  c2w_vm_yield();
  dev->flush_tx();
  net_flush_rx();
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
