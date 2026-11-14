package net

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ErrConnClosed is returned when sending on a connection that has been closed.
var ErrConnClosed = errors.New("gurimoon/net: connection closed")

// Conn wraps a net.Conn with EVEmu stream framing.
// Incoming bytes are fed through StreamPacketizer; outgoing payloads are
// wrapped with a 4-byte LE length header before writing.
type Conn struct {
	raw    net.Conn
	pktz   StreamPacketizer
	sendMu sync.Mutex
	buf    [4096]byte
	closed int32 // atomic flag: 1 = closed
}

// NewConn wraps an existing net.Conn. The caller retains ownership of c and
// must call Close when done.
func NewConn(c net.Conn) *Conn {
	return &Conn{raw: c}
}

// Dial opens a TCP connection to addr and returns a wrapped Conn.
func Dial(addr string) (*Conn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return NewConn(c), nil
}

// ReadPacket reads bytes from the underlying connection until at least one
// complete framed packet is available, then returns it.
// Returns io.EOF when the remote side closes.
func (c *Conn) ReadPacket() ([]byte, error) {
	for {
		if pkt := c.pktz.Pop(); pkt != nil {
			return pkt, nil
		}
		n, err := c.raw.Read(c.buf[:])
		if n > 0 {
			if ferr := c.pktz.Append(c.buf[:n]); ferr != nil {
				return nil, ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}
	}
}

// WriteTimeout bounds one socket write; a stalled peer would otherwise block the system tick that
// flushes to it. Set from server.writeTimeoutSec before any connection exists.
// See docs/support-notes.md#socket-write-deadline.
var WriteTimeout = 15 * time.Second

// SendPacket writes a length-framed payload; concurrent sends are serialized. Returns
// ErrConnClosed on a closed connection. A write past WriteTimeout closes the connection: the
// stream is at an unknown offset, and the reader's next failure runs the disconnect defers.
func (c *Conn) SendPacket(payload []byte) error {
	if atomic.LoadInt32(&c.closed) != 0 {
		return ErrConnClosed
	}
	if len(payload) > MaxPacketSize {
		return ErrPacketTooLarge
	}
	framed := Frame(payload)
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	// Deadline set inside the lock: it is a property of this write, and a
	// concurrent sender setting its own would otherwise shorten ours.
	_ = c.raw.SetWriteDeadline(time.Now().Add(WriteTimeout))
	_, err := c.raw.Write(framed)
	_ = c.raw.SetWriteDeadline(time.Time{})
	if errors.Is(err, os.ErrDeadlineExceeded) {
		atomic.StoreInt32(&c.closed, 1)
		_ = c.raw.Close()
	}
	return err
}

// Close marks the connection as closed and closes the underlying connection.
// Safe to call multiple times.
func (c *Conn) Close() error {
	atomic.StoreInt32(&c.closed, 1)
	return c.raw.Close()
}

// IsClosed reports whether Close has been called on this connection.
func (c *Conn) IsClosed() bool {
	return atomic.LoadInt32(&c.closed) != 0
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.raw.RemoteAddr()
}
