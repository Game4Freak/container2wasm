//! User-space network stack behind the guest's virtio-net device.
//!
//! The guest is on a private ethernet segment (10.0.2.0/24, like QEMU's
//! user networking). This module implements the other end of the wire with
//! smoltcp: it answers ARP, terminates every TCP connection the guest opens
//! and proxies it to a `wasi:sockets` TCP socket of the host (so the host
//! only needs to provide the standard WASI imports), and serves DNS on
//! 10.0.2.3 through `wasi:sockets/ip-name-lookup`. Frames are exchanged
//! with the emulator through `c2w:bridge/vm.net-recv` / `net-send`.
//!
//! Everything runs as tasks of the `wasi:cli/run` export:
//!
//! * `frame_pump` waits for frames from the guest and queues them;
//! * `driver` polls smoltcp, moves data between the guest-side TCP sockets
//!   and the per-connection buffers, flushes frames to the guest and sleeps
//!   until a frame arrives, a connection task signals progress or a smoltcp
//!   timer expires;
//! * one reader and one writer task per connection talk to the host socket;
//! * one task per DNS query resolves the name on the host.
//!
//! Addresses are documented in docs/wasip3.md.

use crate::generated::c2w::bridge::vm;
use crate::generated::wasi::clocks::monotonic_clock;
use crate::generated::wasi::sockets::ip_name_lookup;
use crate::generated::wasi::sockets::types::{
    IpAddress, IpAddressFamily, IpSocketAddress, Ipv4SocketAddress, TcpSocket,
};
use crate::generated::wit_stream;
use smoltcp::iface::{Config, Interface, PollResult, SocketHandle, SocketSet};
use smoltcp::phy::{
    Checksum, ChecksumCapabilities, Device, DeviceCapabilities, Medium, RxToken, TxToken,
};
use smoltcp::socket::{tcp, udp};
use smoltcp::time::{Duration, Instant};
use smoltcp::wire::{
    EthernetAddress, EthernetFrame, EthernetProtocol, HardwareAddress, IpCidr, IpEndpoint,
    IpListenEndpoint, IpProtocol, Ipv4Address, Ipv4Cidr, Ipv4Packet, TcpPacket,
};
use std::cell::{Cell, RefCell};
use std::collections::{BTreeMap, VecDeque};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

/// Debug line on the emulator's stderr (shown with C2W_BRIDGE_DEBUG).
fn debug(args: std::fmt::Arguments<'_>) {
    vm::log(&std::fmt::format(args));
}
macro_rules! debug {
    ($($arg:tt)*) => { debug(format_args!($($arg)*)) };
}

/// Address of the stack (the guest's default gateway).
pub const GATEWAY: Ipv4Address = Ipv4Address::new(10, 0, 2, 2);
/// Address answering DNS queries.
pub const DNS_SERVER: Ipv4Address = Ipv4Address::new(10, 0, 2, 3);
const PREFIX_LEN: u8 = 24;
const STACK_MAC: EthernetAddress = EthernetAddress([0x52, 0x55, 0x0a, 0x00, 0x02, 0x02]);
const MTU: usize = 1514;
const TCP_BUFFER: usize = 64 * 1024;
/// Bytes buffered towards the guest per connection before the reader task
/// stops reading from the host.
const GUEST_BACKLOG: usize = 128 * 1024;
const MAX_CONNECTIONS: usize = 256;
const READ_CHUNK: usize = 32 * 1024;

// ---------------------------------------------------------------------------
// A minimal single-waiter notification for tasks sharing the thread.
// ---------------------------------------------------------------------------

#[derive(Default)]
pub struct Notify {
    pending: Cell<bool>,
    waker: RefCell<Option<Waker>>,
}

impl Notify {
    pub fn notify(&self) {
        self.pending.set(true);
        if let Some(w) = self.waker.borrow_mut().take() {
            w.wake();
        }
    }

    pub fn wait(&self) -> NotifyWait<'_> {
        NotifyWait { n: self }
    }
}

pub struct NotifyWait<'a> {
    n: &'a Notify,
}

impl Future for NotifyWait<'_> {
    type Output = ();
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.n.pending.replace(false) {
            Poll::Ready(())
        } else {
            *self.n.waker.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }
    }
}

// ---------------------------------------------------------------------------
// smoltcp device fed by frame queues
// ---------------------------------------------------------------------------

#[derive(Default)]
struct FrameDevice {
    rx: VecDeque<Vec<u8>>,
    tx: Vec<Vec<u8>>,
}

struct FrameRx(Vec<u8>);

impl RxToken for FrameRx {
    fn consume<R, F: FnOnce(&[u8]) -> R>(self, f: F) -> R {
        f(&self.0)
    }
}

struct FrameTx<'a>(&'a mut Vec<Vec<u8>>);

impl TxToken for FrameTx<'_> {
    fn consume<R, F: FnOnce(&mut [u8]) -> R>(self, len: usize, f: F) -> R {
        let mut buf = vec![0u8; len];
        let r = f(&mut buf);
        self.0.push(buf);
        r
    }
}

impl Device for FrameDevice {
    type RxToken<'a> = FrameRx;
    type TxToken<'a> = FrameTx<'a>;

    fn receive(&mut self, _now: Instant) -> Option<(FrameRx, FrameTx<'_>)> {
        let f = self.rx.pop_front()?;
        Some((FrameRx(f), FrameTx(&mut self.tx)))
    }

    fn transmit(&mut self, _now: Instant) -> Option<FrameTx<'_>> {
        Some(FrameTx(&mut self.tx))
    }

    fn capabilities(&self) -> DeviceCapabilities {
        let mut caps = DeviceCapabilities::default();
        caps.medium = Medium::Ethernet;
        caps.max_transmission_unit = MTU;
        caps.max_burst_size = Some(32);
        let mut cs = ChecksumCapabilities::default();
        // The guest's virtio-net has no offloads: every checksum travels.
        cs.ipv4 = Checksum::Both;
        cs.tcp = Checksum::Both;
        cs.udp = Checksum::Both;
        cs.icmpv4 = Checksum::Both;
        caps.checksum = cs;
        caps
    }
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

/// State shared between the driver and the host-side tasks of one
/// connection. Never borrowed across an await.
#[derive(Default)]
struct ConnState {
    /// Data read from the host, not yet handed to the guest socket.
    to_guest: VecDeque<u8>,
    /// Data read from the guest socket, not yet written to the host.
    to_host: VecDeque<u8>,
    /// The host side reached end of stream (or failed).
    host_eof: bool,
    /// The host connection failed: reset the guest side.
    host_failed: bool,
    /// The guest closed its sending direction.
    guest_eof: bool,
    /// The guest side is gone (reset or fully closed).
    guest_gone: bool,
    /// Task reading from the host, cancelled when the guest side is gone.
    reader_task: Option<wit_bindgen::JoinHandle<()>>,
}

#[derive(Default)]
struct Conn {
    state: RefCell<ConnState>,
    /// Wakes the writer task (data for the host, or guest EOF).
    writer: Notify,
    /// Wakes the reader task when `to_guest` drained below the backlog.
    reader: Notify,
}

type ConnRef = Rc<Conn>;

struct Connection {
    handle: SocketHandle,
    conn: ConnRef,
}

struct DnsResponse {
    endpoint: IpEndpoint,
    response: Vec<u8>,
}

/// The stack and the notification waking its driver.
pub struct Net {
    stack: RefCell<Stack>,
    wake: Notify,
}

type NetRef = Rc<Net>;

pub struct Stack {
    device: FrameDevice,
    iface: Interface,
    sockets: SocketSet<'static>,
    /// Listening sockets created for a SYN, keyed by destination.
    listeners: BTreeMap<(Ipv4Address, u16), SocketHandle>,
    connections: Vec<Connection>,
    dns_socket: SocketHandle,
    dns_responses: Vec<DnsResponse>,
    start: u64,
}

impl Stack {
    fn new() -> Stack {
        let mut device = FrameDevice::default();
        let start = monotonic_clock::now();
        let mut config = Config::new(HardwareAddress::Ethernet(STACK_MAC));
        config.random_seed = start;
        let mut iface = Interface::new(config, &mut device, Instant::ZERO);
        iface.update_ip_addrs(|addrs| {
            addrs
                .push(IpCidr::Ipv4(Ipv4Cidr::new(GATEWAY, PREFIX_LEN)))
                .unwrap();
        });
        // Accept packets for any destination: every address the guest talks
        // to terminates here and is proxied to the host.
        iface.set_any_ip(true);
        let mut sockets = SocketSet::new(Vec::new());
        let dns_socket = {
            let rx =
                udp::PacketBuffer::new(vec![udp::PacketMetadata::EMPTY; 16], vec![0; 16 * 1024]);
            let tx =
                udp::PacketBuffer::new(vec![udp::PacketMetadata::EMPTY; 16], vec![0; 16 * 1024]);
            let mut s = udp::Socket::new(rx, tx);
            s.bind(IpListenEndpoint {
                addr: Some(DNS_SERVER.into()),
                port: 53,
            })
            .unwrap();
            sockets.add(s)
        };
        Stack {
            device,
            iface,
            sockets,
            listeners: BTreeMap::new(),
            connections: Vec::new(),
            dns_socket,
            dns_responses: Vec::new(),
            start,
        }
    }

    fn now(&self) -> Instant {
        Instant::from_micros((monotonic_clock::now().saturating_sub(self.start) / 1000) as i64)
    }

    /// Queue frames received from the guest (`net-recv` encoding).
    ///
    /// Frames are processed one at a time so that a SYN always finds a
    /// listening socket of its own: two connections to the same destination
    /// in one batch would otherwise share a listener, which can accept only
    /// one of them (the other gets a reset).
    fn push_frames(&mut self, net: &NetRef, batch: &[u8]) {
        let now = self.now();
        let mut off = 0;
        while off + 2 <= batch.len() {
            let len = batch[off] as usize | ((batch[off + 1] as usize) << 8);
            off += 2;
            if len == 0 || off + len > batch.len() {
                break;
            }
            let frame = &batch[off..off + len];
            off += len;
            let syn = self.inspect_frame(frame);
            self.device.rx.push_back(frame.to_vec());
            if syn {
                self.iface
                    .poll_ingress_single(now, &mut self.device, &mut self.sockets);
                self.adopt_listeners(net);
            }
        }
    }

    /// Make sure a listening socket exists for the destination of a TCP SYN
    /// before smoltcp processes the frame. Returns true for a SYN.
    fn inspect_frame(&mut self, frame: &[u8]) -> bool {
        let Ok(eth) = EthernetFrame::new_checked(frame) else {
            return false;
        };
        if eth.ethertype() != EthernetProtocol::Ipv4 {
            return false;
        }
        let Ok(ip) = Ipv4Packet::new_checked(eth.payload()) else {
            return false;
        };
        if ip.next_header() != IpProtocol::Tcp {
            return false;
        }
        let Ok(tcp) = TcpPacket::new_checked(ip.payload()) else {
            return false;
        };
        if !tcp.syn() || tcp.ack() {
            return false;
        }
        let key = (ip.dst_addr(), tcp.dst_port());
        if let Some(h) = self.listeners.get(&key) {
            if self.sockets.get::<tcp::Socket>(*h).state() == tcp::State::Listen {
                return true;
            }
        }
        if self.connections.len() + self.listeners.len() >= MAX_CONNECTIONS {
            return true;
        }
        let mut s = tcp::Socket::new(
            tcp::SocketBuffer::new(vec![0; TCP_BUFFER]),
            tcp::SocketBuffer::new(vec![0; TCP_BUFFER]),
        );
        s.set_nagle_enabled(false);
        s.set_ack_delay(None);
        if s.listen(IpListenEndpoint {
            addr: Some(key.0.into()),
            port: key.1,
        })
        .is_ok()
        {
            let h = self.sockets.add(s);
            self.listeners.insert(key, h);
            debug!("net: SYN to {}:{}", key.0, key.1);
        }
        true
    }

    /// Run smoltcp and the proxy logic. Returns the frames to send to the
    /// guest (`net-send` encoding) and the delay until the next timer.
    fn run(&mut self, net: &NetRef) -> (Vec<u8>, Option<Duration>) {
        let now = self.now();
        loop {
            let r = self.iface.poll(now, &mut self.device, &mut self.sockets);
            self.adopt_listeners(net);
            let changed = self.service_connections();
            let dns = self.service_dns(net);
            if r == PollResult::None && !changed && !dns {
                break;
            }
        }
        let mut out = Vec::new();
        for f in self.device.tx.drain(..) {
            out.push((f.len() & 0xff) as u8);
            out.push((f.len() >> 8) as u8);
            out.extend_from_slice(&f);
        }
        let delay = self.iface.poll_delay(now, &self.sockets);
        (out, delay)
    }

    /// Listening sockets that received a SYN become connections.
    fn adopt_listeners(&mut self, net: &NetRef) {
        let mut adopted = Vec::new();
        for (key, h) in self.listeners.iter() {
            if self.sockets.get::<tcp::Socket>(*h).state() != tcp::State::Listen {
                adopted.push((*key, *h));
            }
        }
        for (key, h) in adopted {
            self.listeners.remove(&key);
            if !self.sockets.get::<tcp::Socket>(h).is_open() {
                self.sockets.remove(h);
                continue;
            }
            let conn: ConnRef = Rc::new(Conn::default());
            debug!(
                "net: connection {:?} to {}:{} ({} open)",
                h,
                key.0,
                key.1,
                self.connections.len() + 1
            );
            self.connections.push(Connection {
                handle: h,
                conn: conn.clone(),
            });
            wit_bindgen::spawn_local(host_connection(net.clone(), conn, key.0, key.1));
        }
    }

    /// Move data between the guest sockets and the connection buffers.
    /// Returns true when socket state may have changed.
    fn service_connections(&mut self) -> bool {
        let mut changed = false;
        let mut buf = vec![0u8; READ_CHUNK];
        let mut i = 0;
        while i < self.connections.len() {
            let handle = self.connections[i].handle;
            let conn = self.connections[i].conn.clone();
            let mut c = conn.state.borrow_mut();
            let s = self.sockets.get_mut::<tcp::Socket>(handle);
            if c.host_failed && s.is_open() {
                s.abort();
                changed = true;
            }
            // host -> guest
            while !c.to_guest.is_empty() && s.can_send() {
                let n = s.send_capacity().min(c.to_guest.len()).min(buf.len());
                for (dst, src) in buf[..n].iter_mut().zip(c.to_guest.iter()) {
                    *dst = *src;
                }
                match s.send_slice(&buf[..n]) {
                    Ok(sent) if sent > 0 => {
                        c.to_guest.drain(..sent);
                        changed = true;
                    }
                    _ => break,
                }
            }
            if c.to_guest.len() < GUEST_BACKLOG {
                conn.reader.notify();
            }
            // guest -> host
            while s.can_recv() {
                match s.recv_slice(&mut buf) {
                    Ok(n) if n > 0 => {
                        c.to_host.extend(buf[..n].iter());
                        conn.writer.notify();
                    }
                    _ => break,
                }
            }
            let guest_closed = matches!(
                s.state(),
                tcp::State::CloseWait
                    | tcp::State::LastAck
                    | tcp::State::Closing
                    | tcp::State::TimeWait
                    | tcp::State::Closed
            );
            if guest_closed && !c.guest_eof && !s.can_recv() {
                // FIN from the guest: tell the host once everything queued
                // has been written.
                c.guest_eof = true;
                conn.writer.notify();
            }
            if c.host_eof && c.to_guest.is_empty() && s.may_send() {
                s.close();
                changed = true;
            }
            if !s.is_open() {
                debug!("net: connection {:?} closed ({:?})", handle, s.state());
                c.guest_gone = true;
                c.guest_eof = true;
                if let Some(mut t) = c.reader_task.take() {
                    t.cancel();
                }
                conn.writer.notify();
                conn.reader.notify();
                drop(c);
                self.sockets.remove(handle);
                self.connections.swap_remove(i);
                changed = true;
                continue;
            }
            drop(c);
            i += 1;
        }
        changed
    }

    /// Answer DNS queries and send the responses that resolved.
    fn service_dns(&mut self, net: &NetRef) -> bool {
        let mut progressed = false;
        let mut buf = [0u8; 2048];
        let mut queries = Vec::new();
        {
            let s = self.sockets.get_mut::<udp::Socket>(self.dns_socket);
            while s.can_recv() {
                match s.recv_slice(&mut buf) {
                    Ok((n, meta)) => queries.push((buf[..n].to_vec(), meta.endpoint)),
                    Err(_) => break,
                }
            }
        }
        for (q, endpoint) in queries {
            progressed = true;
            match dns::parse_query(&q) {
                Some(query) if query.qtype == dns::TYPE_A && query.qclass == dns::CLASS_IN => {
                    wit_bindgen::spawn_local(resolve(net.clone(), query, endpoint));
                }
                Some(query) => {
                    // No IPv6 on the guest network: AAAA (and anything else)
                    // gets an empty answer section.
                    let response = dns::build_response(&query, &[], dns::RCODE_NOERROR);
                    self.dns_responses.push(DnsResponse { endpoint, response });
                }
                None => {}
            }
        }
        let responses: Vec<DnsResponse> = self.dns_responses.drain(..).collect();
        let s = self.sockets.get_mut::<udp::Socket>(self.dns_socket);
        for r in responses {
            progressed = true;
            let _ = s.send_slice(&r.response, r.endpoint);
        }
        progressed
    }
}

// ---------------------------------------------------------------------------
// Host side tasks
// ---------------------------------------------------------------------------

fn socket_address(ip: Ipv4Address, port: u16) -> IpSocketAddress {
    let o = ip.octets();
    IpSocketAddress::Ipv4(Ipv4SocketAddress {
        port,
        address: (o[0], o[1], o[2], o[3]),
    })
}

/// Connect to `ip:port` on the host and pump data in both directions.
async fn host_connection(net: NetRef, conn: ConnRef, ip: Ipv4Address, port: u16) {
    let socket = match TcpSocket::create(IpAddressFamily::Ipv4) {
        Ok(s) => s,
        Err(e) => {
            debug!("net: tcp-socket.create failed: {:?}", e);
            return host_failed(&net, &conn);
        }
    };
    if let Err(e) = socket.connect(socket_address(ip, port)).await {
        debug!("net: connect to {}:{} failed: {:?}", ip, port, e);
        return host_failed(&net, &conn);
    }
    debug!("net: connected to {}:{}", ip, port);
    if conn.state.borrow().guest_gone {
        return;
    }
    let (rx_stream, _rx_result) = socket.receive();
    let (tx_writer, tx_reader) = wit_stream::new::<u8>();
    let _tx_result = socket.send(tx_reader);
    let socket = Rc::new(socket);

    let reader = wit_bindgen::spawn_local(host_reader(
        net.clone(),
        conn.clone(),
        rx_stream,
        socket.clone(),
    ));
    conn.state.borrow_mut().reader_task = Some(reader);
    let writer =
        wit_bindgen::spawn_local(host_writer(net.clone(), conn.clone(), tx_writer, socket));
    let _ = writer.await;
}

fn host_failed(net: &NetRef, conn: &ConnRef) {
    conn.state.borrow_mut().host_failed = true;
    net.wake.notify();
}

async fn host_reader(
    net: NetRef,
    conn: ConnRef,
    mut rx: wit_bindgen::StreamReader<u8>,
    _socket: Rc<TcpSocket>,
) {
    loop {
        // Back-pressure: wait until the guest consumed its backlog.
        loop {
            {
                let c = conn.state.borrow();
                if c.guest_gone {
                    return;
                }
                if c.to_guest.len() < GUEST_BACKLOG {
                    break;
                }
            }
            conn.reader.wait().await;
        }
        // The read fills the spare capacity of the buffer.
        let (result, data) = rx.read(Vec::with_capacity(READ_CHUNK)).await;
        let mut c = conn.state.borrow_mut();
        match result {
            wit_bindgen::StreamResult::Complete(n) if n > 0 => {
                c.to_guest.extend(data[..n].iter());
            }
            other => {
                debug!("net: host read ended: {:?}", other);
                c.host_eof = true;
                drop(c);
                net.wake.notify();
                return;
            }
        }
        drop(c);
        net.wake.notify();
    }
}

async fn host_writer(
    net: NetRef,
    conn: ConnRef,
    mut tx: wit_bindgen::StreamWriter<u8>,
    _socket: Rc<TcpSocket>,
) {
    loop {
        let (chunk, eof) = {
            let mut c = conn.state.borrow_mut();
            let chunk: Vec<u8> = c.to_host.drain(..).collect();
            (chunk, c.guest_eof || c.guest_gone)
        };
        if !chunk.is_empty() {
            let n = chunk.len();
            let rest = tx.write_all(chunk).await;
            if !rest.is_empty() {
                debug!(
                    "net: host write ended ({} of {} bytes unsent)",
                    rest.len(),
                    n
                );
                // The host closed its receiving side.
                conn.state.borrow_mut().host_eof = true;
                net.wake.notify();
                return;
            }
            continue;
        }
        if eof {
            // Dropping the writer closes the host's sending direction.
            return;
        }
        conn.writer.wait().await;
    }
}

/// Resolve a DNS A query through the host and queue the response.
async fn resolve(net: NetRef, query: dns::Query, endpoint: IpEndpoint) {
    debug!("net: resolve {}", query.name);
    let (addrs, rcode) = match ip_name_lookup::resolve_addresses(query.name.clone()).await {
        Ok(list) => {
            let v4: Vec<[u8; 4]> = list
                .into_iter()
                .filter_map(|a| match a {
                    IpAddress::Ipv4((a, b, c, d)) => Some([a, b, c, d]),
                    _ => None,
                })
                .collect();
            (v4, dns::RCODE_NOERROR)
        }
        Err(_) => (Vec::new(), dns::RCODE_NXDOMAIN),
    };
    let response = dns::build_response(&query, &addrs, rcode);
    net.stack
        .borrow_mut()
        .dns_responses
        .push(DnsResponse { endpoint, response });
    net.wake.notify();
}

// ---------------------------------------------------------------------------
// Driver tasks
// ---------------------------------------------------------------------------

/// Receive frames from the guest and hand them to the stack.
async fn frame_pump(net: NetRef) {
    loop {
        let batch = vm::net_recv().await;
        net.stack.borrow_mut().push_frames(&net, &batch);
        net.wake.notify();
    }
}

/// Timer future resolving after `d`, or never when `None`.
async fn sleep(d: Option<Duration>) {
    match d {
        Some(d) => {
            let micros = d.total_micros().max(1000);
            monotonic_clock::wait_for(micros * 1000).await
        }
        None => std::future::pending::<()>().await,
    }
}

/// Poll the stack whenever something happened.
async fn driver(net: NetRef) {
    loop {
        let (frames, delay) = net.stack.borrow_mut().run(&net);
        if !frames.is_empty() {
            vm::net_send(&frames);
        }
        let notified = net.wake.wait();
        let timer = sleep(delay);
        futures::pin_mut!(notified);
        futures::pin_mut!(timer);
        let _ = futures::future::select(notified, timer).await;
    }
}

/// Start the network stack. Called once from `run`.
pub fn start() {
    let net: NetRef = Rc::new(Net {
        stack: RefCell::new(Stack::new()),
        wake: Notify::default(),
    });
    wit_bindgen::spawn_local(frame_pump(net.clone()));
    wit_bindgen::spawn_local(driver(net));
}

// ---------------------------------------------------------------------------
// DNS wire format (queries with one question, A answers)
// ---------------------------------------------------------------------------

mod dns {
    pub const TYPE_A: u16 = 1;
    pub const CLASS_IN: u16 = 1;
    pub const RCODE_NOERROR: u8 = 0;
    pub const RCODE_NXDOMAIN: u8 = 3;

    pub struct Query {
        pub id: u16,
        pub name: String,
        pub qtype: u16,
        pub qclass: u16,
        /// The question section, copied verbatim into the response.
        pub question: Vec<u8>,
    }

    pub fn parse_query(msg: &[u8]) -> Option<Query> {
        if msg.len() < 12 {
            return None;
        }
        let id = u16::from_be_bytes([msg[0], msg[1]]);
        let flags = u16::from_be_bytes([msg[2], msg[3]]);
        if flags & 0x8000 != 0 {
            return None; // a response
        }
        let qdcount = u16::from_be_bytes([msg[4], msg[5]]);
        if qdcount != 1 {
            return None;
        }
        let mut pos = 12;
        let mut labels: Vec<String> = Vec::new();
        loop {
            let len = *msg.get(pos)? as usize;
            pos += 1;
            if len == 0 {
                break;
            }
            if len & 0xc0 != 0 || pos + len > msg.len() {
                return None; // compression is not used in questions
            }
            labels.push(String::from_utf8_lossy(&msg[pos..pos + len]).into_owned());
            pos += len;
        }
        if pos + 4 > msg.len() {
            return None;
        }
        let qtype = u16::from_be_bytes([msg[pos], msg[pos + 1]]);
        let qclass = u16::from_be_bytes([msg[pos + 2], msg[pos + 3]]);
        pos += 4;
        Some(Query {
            id,
            name: labels.join("."),
            qtype,
            qclass,
            question: msg[12..pos].to_vec(),
        })
    }

    pub fn build_response(q: &Query, addrs: &[[u8; 4]], rcode: u8) -> Vec<u8> {
        let mut out = Vec::with_capacity(12 + q.question.len() + addrs.len() * 16);
        out.extend_from_slice(&q.id.to_be_bytes());
        // QR=1, RD=1, RA=1
        out.extend_from_slice(&(0x8180u16 | rcode as u16).to_be_bytes());
        out.extend_from_slice(&1u16.to_be_bytes());
        out.extend_from_slice(&(addrs.len() as u16).to_be_bytes());
        out.extend_from_slice(&0u16.to_be_bytes());
        out.extend_from_slice(&0u16.to_be_bytes());
        out.extend_from_slice(&q.question);
        for a in addrs {
            out.extend_from_slice(&[0xc0, 0x0c]); // pointer to the question name
            out.extend_from_slice(&TYPE_A.to_be_bytes());
            out.extend_from_slice(&q.qclass.to_be_bytes());
            out.extend_from_slice(&60u32.to_be_bytes()); // TTL
            out.extend_from_slice(&4u16.to_be_bytes());
            out.extend_from_slice(a);
        }
        out
    }
}
