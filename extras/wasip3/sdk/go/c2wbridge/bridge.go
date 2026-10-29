// Package c2wbridge lets programs running inside a container2wasm WASI 0.3
// image call the WIT interfaces imported by the component, and serve the
// interfaces it exports.
//
// It speaks the newline delimited JSON protocol of the bridge socket
// (/run/c2w/bridge.sock) described in docs/wasip3-bridge-protocol.md.
// Values are mapped between WIT and JSON as documented there; this package
// only transports JSON, so arguments and results are plain Go values
// marshalled with encoding/json.
package c2wbridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

// DefaultSocket is the path of the bridge socket inside the container.
const DefaultSocket = "/run/c2w/bridge.sock"

// Handler implements the functions of an exported interface. It receives the
// function name and the JSON encoded arguments and returns a value that is
// marshalled with encoding/json as the result.
type Handler func(function string, args []json.RawMessage) (any, error)

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *string         `json:"error,omitempty"`
}

// Client is a connection to the bridge. It is safe for concurrent use.
type Client struct {
	conn    net.Conn
	writeMu sync.Mutex

	nextID  uint64
	mu      sync.Mutex
	pending map[string]chan *message
	closed  chan struct{}
	err     error

	handlersMu sync.RWMutex
	handlers   map[string]Handler
}

// Dial connects to the bridge socket. The C2W_BRIDGE_SOCKET environment
// variable overrides the default path.
func Dial() (*Client, error) {
	path := os.Getenv("C2W_BRIDGE_SOCKET")
	if path == "" {
		path = DefaultSocket
	}
	return DialPath(path)
}

// DialPath connects to the bridge socket at path.
func DialPath(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("c2wbridge: %w", err)
	}
	c := &Client{
		conn:     conn,
		pending:  make(map[string]chan *message),
		closed:   make(chan struct{}),
		handlers: make(map[string]Handler),
	}
	go c.readLoop()
	return c, nil
}

// Close closes the connection. Interfaces served by this client stop being
// available to the host.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) readLoop() {
	r := bufio.NewReaderSize(c.conn, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			c.err = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			close(c.closed)
			return
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if m.Method == "invoke" {
			go c.handleInvoke(&m)
			continue
		}
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- &m
		}
	}
}

func (c *Client) send(m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.conn.Write(append(b, '\n'))
	return err
}

func (c *Client) request(method string, params any) (json.RawMessage, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := json.RawMessage(fmt.Sprintf("%d", atomic.AddUint64(&c.nextID, 1)))
	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	c.pending[string(id)] = ch
	c.mu.Unlock()
	if err := c.send(&message{ID: id, Method: method, Params: p}); err != nil {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return nil, err
	}
	m, ok := <-ch
	if !ok {
		return nil, errors.New("c2wbridge: connection closed")
	}
	if m.Error != nil {
		return nil, errors.New(*m.Error)
	}
	if m.Result == nil {
		return json.RawMessage("null"), nil
	}
	return m.Result, nil
}

// Call invokes function `function` of the imported interface `iface`
// (for example "wasi:clocks/system-clock@0.3.0") with the given arguments
// and returns the JSON encoded result.
func (c *Client) Call(iface, function string, args ...any) (json.RawMessage, error) {
	if args == nil {
		args = []any{}
	}
	return c.request("call", map[string]any{"interface": iface, "function": function, "args": args})
}

// CallInto is like Call but unmarshals the result into result.
func (c *Client) CallInto(result any, iface, function string, args ...any) error {
	raw, err := c.Call(iface, function, args...)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(raw, result)
}

// Describe returns the description of the WIT world of the component: the
// JSON produced by `wasm-tools component wit --json`.
func (c *Client) Describe() (json.RawMessage, error) {
	return c.request("describe", nil)
}

// Serve registers handler for the exported interface `iface`. Calls made
// by the host to that interface are routed to handler until the client is
// closed. Only one client can serve an interface at a time.
func (c *Client) Serve(iface string, handler Handler) error {
	c.handlersMu.Lock()
	c.handlers[iface] = handler
	c.handlersMu.Unlock()
	_, err := c.request("serve", map[string]string{"interface": iface})
	return err
}

// Wait blocks until the connection is closed.
func (c *Client) Wait() error {
	<-c.closed
	return c.err
}

func (c *Client) handleInvoke(m *message) {
	var req struct {
		Interface string            `json:"interface"`
		Function  string            `json:"function"`
		Args      []json.RawMessage `json:"args"`
	}
	reply := &message{ID: m.ID}
	if err := json.Unmarshal(m.Params, &req); err != nil {
		reply.Error = strPtr("invalid invoke request: " + err.Error())
	} else {
		c.handlersMu.RLock()
		h := c.handlers[req.Interface]
		c.handlersMu.RUnlock()
		if h == nil {
			reply.Error = strPtr("no handler for " + req.Interface)
		} else if res, err := h(req.Function, req.Args); err != nil {
			reply.Error = strPtr(err.Error())
		} else if b, err := json.Marshal(res); err != nil {
			reply.Error = strPtr("cannot encode result: " + err.Error())
		} else {
			reply.Result = b
		}
	}
	_ = c.send(reply)
}

func strPtr(s string) *string { return &s }
