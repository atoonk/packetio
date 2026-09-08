//go:build linux

package netstack

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
)

// The gVisor adapters return their own errors, flattened to text, so a
// caller cannot match them: an accept that failed because the listener
// closed does not satisfy errors.Is(err, net.ErrClosed), and a read that
// hit its deadline does not satisfy os.ErrDeadlineExceeded. Both are what
// ordinary Go code tests for -- a server's accept loop is written around
// the first -- so the listener and connection this package hands out are
// thin wrappers that supply them, from what they know about their own
// state rather than from the text.

// listener is a net.Listener whose Accept reports a closed listener, or a
// closed stack, as net.ErrClosed.
type listener struct {
	net.Listener
	st     *Stack
	closed atomic.Bool
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, l.closedErr("accept", err)
	}
	return &conn{Conn: c}, nil
}

func (l *listener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

func (l *listener) closedErr(op string, err error) error {
	if !l.closed.Load() && !l.st.closed.Load() {
		return err
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		oe.Err = net.ErrClosed
		return oe
	}
	return &net.OpError{Op: op, Net: "tcp", Addr: l.Addr(), Err: net.ErrClosed}
}

// conn is a net.Conn whose errors after Close say so, and whose deadline
// errors match os.ErrDeadlineExceeded as the net package's do.
type conn struct {
	net.Conn
	closed atomic.Bool
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, c.mapErr(err)
}

func (c *conn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	return n, c.mapErr(err)
}

func (c *conn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func (c *conn) mapErr(err error) error {
	if err == nil {
		return err
	}
	var oe *net.OpError
	if !errors.As(err, &oe) {
		return err
	}
	switch {
	case oe.Timeout():
		oe.Err = os.ErrDeadlineExceeded
	case c.closed.Load():
		oe.Err = net.ErrClosed
	default:
		return err
	}
	return oe
}
