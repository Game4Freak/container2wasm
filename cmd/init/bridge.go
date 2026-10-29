package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Bridge between programs running in the container and the WIT interfaces
// of the WASI 0.3 component (see docs/wasip3-bridge-protocol.md).
//
// Transport to the emulator: binary frames over a dedicated virtio-console
// port (bridgeDevice). Transport to the container: newline delimited JSON
// messages over a unix socket (bridgeSocket) that is bind mounted into the
// container together with the c2w-call command (a symlink to this binary).

const (
	bridgeDevice  = "/dev/hvc1"
	bridgeDir     = "/run/c2w"
	bridgeSocket  = "/run/c2w/bridge.sock"
	bridgeCallCmd = "/run/c2w/c2w-call"

	frameMagic   = 0x42573243 // "C2WB"
	frameVersion = 1

	frameHello    = 1
	frameDescribe = 2
	frameCall     = 3
	frameResult   = 4
	frameInvoke   = 5
	frameReturn   = 6

	frameHeaderSize = 16
	frameMaxPayload = 64 << 20
)

type frame struct {
	typ     uint8
	id      uint32
	payload []byte
}

// bridgeServer multiplexes the clients of the unix socket onto the emulator
// port.
type bridgeServer struct {
	port        *os.File
	writeMu     sync.Mutex
	description []byte
	descReady   chan struct{}
	descOnce    sync.Once

	nextCallID uint32
	callsMu    sync.Mutex
	calls      map[uint32]chan *frame // CALL id -> RESULT

	handlersMu sync.Mutex
	handlers   map[string]*bridgeClient // interface name -> serving client
}

func newBridgeServer(port *os.File) *bridgeServer {
	return &bridgeServer{
		port:      port,
		descReady: make(chan struct{}),
		calls:     make(map[uint32]chan *frame),
		handlers:  make(map[string]*bridgeClient),
	}
}

// startBridge opens the emulator port and serves the unix socket. It
// returns once the handshake with the emulator completed.
func startBridge() error {
	port, err := os.OpenFile(bridgeDevice, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open bridge device: %w", err)
	}
	if err := setRawTTY(port); err != nil {
		return fmt.Errorf("failed to configure bridge device: %w", err)
	}
	s := newBridgeServer(port)
	go s.readLoop()
	if err := s.writeFrame(frameHello, 0, nil); err != nil {
		return err
	}
	<-s.descReady

	if err := os.MkdirAll(bridgeDir, 0o755); err != nil {
		return err
	}
	os.Remove(bridgeSocket)
	l, err := net.Listen("unix", bridgeSocket)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", bridgeSocket, err)
	}
	if err := os.Chmod(bridgeSocket, 0o666); err != nil {
		return err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				log.Printf("bridge: accept: %v", err)
				return
			}
			go s.serveClient(c)
		}
	}()
	log.Printf("bridge: serving %s", bridgeSocket)
	return nil
}

// setRawTTY disables the line discipline processing of the console port.
func setRawTTY(f *os.File) error {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t))); e != 0 {
		return e
	}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t))); e != 0 {
		return e
	}
	return nil
}

func (s *bridgeServer) writeFrame(typ uint8, id uint32, payload []byte) error {
	if len(payload) > frameMaxPayload {
		return fmt.Errorf("bridge payload too large (%d bytes)", len(payload))
	}
	buf := make([]byte, frameHeaderSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:], frameMagic)
	buf[4] = frameVersion
	buf[5] = typ
	binary.LittleEndian.PutUint32(buf[8:], id)
	binary.LittleEndian.PutUint32(buf[12:], uint32(len(payload)))
	copy(buf[frameHeaderSize:], payload)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for len(buf) > 0 {
		n, err := s.port.Write(buf)
		if err != nil {
			return fmt.Errorf("bridge write: %w", err)
		}
		buf = buf[n:]
	}
	return nil
}

func readFrame(r *bufio.Reader) (*frame, error) {
	var hdr [frameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(hdr[0:]) != frameMagic || hdr[4] != frameVersion {
		return nil, errors.New("bad frame header from emulator")
	}
	n := binary.LittleEndian.Uint32(hdr[12:])
	if n > frameMaxPayload {
		return nil, fmt.Errorf("frame payload too large (%d bytes)", n)
	}
	f := &frame{typ: hdr[5], id: binary.LittleEndian.Uint32(hdr[8:]), payload: make([]byte, n)}
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *bridgeServer) readLoop() {
	r := bufio.NewReaderSize(s.port, 1<<16)
	for {
		f, err := readFrame(r)
		if err != nil {
			log.Printf("bridge: read: %v", err)
			return
		}
		switch f.typ {
		case frameDescribe:
			s.descOnce.Do(func() {
				s.description = f.payload
				close(s.descReady)
			})
		case frameResult:
			s.callsMu.Lock()
			ch := s.calls[f.id]
			delete(s.calls, f.id)
			s.callsMu.Unlock()
			if ch != nil {
				ch <- f
			}
		case frameInvoke:
			go s.handleInvoke(f)
		default:
			log.Printf("bridge: unexpected frame type %d", f.typ)
		}
	}
}

// call performs a host function call. request is the JSON encoded
// {"interface","function","args"} document.
func (s *bridgeServer) call(request []byte) (json.RawMessage, error) {
	id := atomic.AddUint32(&s.nextCallID, 1)
	ch := make(chan *frame, 1)
	s.callsMu.Lock()
	s.calls[id] = ch
	s.callsMu.Unlock()
	if err := s.writeFrame(frameCall, id, request); err != nil {
		s.callsMu.Lock()
		delete(s.calls, id)
		s.callsMu.Unlock()
		return nil, err
	}
	f := <-ch
	return parseResponse(f.payload)
}

// parseResponse decodes {"result":..} or {"error":".."}.
func parseResponse(payload []byte) (json.RawMessage, error) {
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *string         `json:"error"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("invalid response from emulator: %w", err)
	}
	if resp.Error != nil {
		return nil, errors.New(*resp.Error)
	}
	if resp.Result == nil {
		return json.RawMessage("null"), nil
	}
	return resp.Result, nil
}

// handleInvoke routes a call from the host to the client serving the
// interface and sends its answer back.
func (s *bridgeServer) handleInvoke(f *frame) {
	var req struct {
		Interface string `json:"interface"`
	}
	resp := []byte(`{"error":"no handler registered in the container"}`)
	if err := json.Unmarshal(f.payload, &req); err != nil {
		resp = jsonError(fmt.Sprintf("invalid invoke request: %v", err))
	} else {
		s.handlersMu.Lock()
		c := s.handlers[req.Interface]
		s.handlersMu.Unlock()
		if c != nil {
			resp = c.invoke(f.payload)
		}
	}
	if err := s.writeFrame(frameReturn, f.id, resp); err != nil {
		log.Printf("bridge: %v", err)
	}
}

func jsonError(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

// ---- unix socket protocol -------------------------------------------------

// rpcMessage is a JSON-RPC 2.0 like message exchanged with clients.
type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *string         `json:"error,omitempty"`
}

type bridgeClient struct {
	s       *bridgeServer
	conn    net.Conn
	writeMu sync.Mutex

	invokeMu sync.Mutex
	nextID   uint64
	pending  map[string]chan *rpcMessage
}

func (s *bridgeServer) serveClient(conn net.Conn) {
	c := &bridgeClient{s: s, conn: conn, pending: make(map[string]chan *rpcMessage)}
	defer func() {
		conn.Close()
		s.handlersMu.Lock()
		for k, v := range s.handlers {
			if v == c {
				delete(s.handlers, k)
			}
		}
		s.handlersMu.Unlock()
		c.invokeMu.Lock()
		for k, ch := range c.pending {
			close(ch)
			delete(c.pending, k)
		}
		c.invokeMu.Unlock()
	}()
	r := bufio.NewReaderSize(conn, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			c.send(&rpcMessage{Error: strPtr("invalid JSON: " + err.Error())})
			continue
		}
		if m.Method == "" {
			// Response to an invoke we sent.
			c.invokeMu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.invokeMu.Unlock()
			if ch != nil {
				ch <- &m
			}
			continue
		}
		go c.handleRequest(&m)
	}
}

func strPtr(s string) *string { return &s }

func (c *bridgeClient) send(m *rpcMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.conn.Write(append(b, '\n'))
	return err
}

func (c *bridgeClient) handleRequest(m *rpcMessage) {
	reply := &rpcMessage{ID: m.ID}
	switch m.Method {
	case "call":
		res, err := c.s.call(m.Params)
		if err != nil {
			reply.Error = strPtr(err.Error())
		} else {
			reply.Result = res
		}
	case "describe":
		reply.Result = c.s.description
	case "serve":
		var p struct {
			Interface string `json:"interface"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil || p.Interface == "" {
			reply.Error = strPtr("serve requires {\"interface\": name}")
		} else {
			c.s.handlersMu.Lock()
			c.s.handlers[p.Interface] = c
			c.s.handlersMu.Unlock()
			reply.Result = json.RawMessage("true")
		}
	default:
		reply.Error = strPtr("unknown method " + m.Method)
	}
	if err := c.send(reply); err != nil {
		log.Printf("bridge: client write: %v", err)
	}
}

// invoke forwards an INVOKE request to the client and returns the JSON
// response document ({"result":..} or {"error":..}).
func (c *bridgeClient) invoke(request []byte) []byte {
	c.invokeMu.Lock()
	c.nextID++
	id := json.RawMessage(fmt.Sprintf("%d", c.nextID))
	ch := make(chan *rpcMessage, 1)
	c.pending[string(id)] = ch
	c.invokeMu.Unlock()
	if err := c.send(&rpcMessage{ID: id, Method: "invoke", Params: request}); err != nil {
		return jsonError("handler unreachable: " + err.Error())
	}
	m, ok := <-ch
	if !ok {
		return jsonError("handler disconnected")
	}
	if m.Error != nil {
		return jsonError(*m.Error)
	}
	res := m.Result
	if res == nil {
		res = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]json.RawMessage{"result": res})
	return b
}

// ---- c2w-call command ---------------------------------------------------------

// runCallCommand implements `c2w-call`, a tiny client of the bridge socket
// for shell scripts and tests:
//
//	c2w-call <interface> <function> [json-args]   call a host function
//	c2w-call --describe                           print the WIT description
func runCallCommand(args []string) int {
	if len(args) == 1 && args[0] == "--describe" {
		res, err := rpcCall("describe", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "c2w-call: %v\n", err)
			return 1
		}
		os.Stdout.Write(res)
		fmt.Println()
		return 0
	}
	if len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: c2w-call <interface> <function> [json-args]\n       c2w-call --describe")
		return 2
	}
	jsonArgs := json.RawMessage("[]")
	if len(args) == 3 {
		jsonArgs = json.RawMessage(args[2])
	}
	params, _ := json.Marshal(map[string]interface{}{"interface": args[0], "function": args[1], "args": jsonArgs})
	res, err := rpcCall("call", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "c2w-call: %v\n", err)
		return 1
	}
	os.Stdout.Write(res)
	fmt.Println()
	return 0
}

func rpcCall(method string, params json.RawMessage) (json.RawMessage, error) {
	sock := os.Getenv("C2W_BRIDGE_SOCKET")
	if sock == "" {
		sock = bridgeSocket
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req, _ := json.Marshal(&rpcMessage{ID: json.RawMessage("1"), Method: method, Params: params})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(conn, 1<<16).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var m rpcMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	if m.Error != nil {
		return nil, errors.New(*m.Error)
	}
	return m.Result, nil
}

// isCallCommand reports whether this binary was invoked as c2w-call.
func isCallCommand() bool {
	return filepath.Base(os.Args[0]) == "c2w-call"
}
