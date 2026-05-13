// Package sync implements the launcher↔worker reconcile-trigger
// protocol used in design §4.3 sync mode.
//
// Wire protocol: newline-delimited messages over a Unix domain socket.
// The launcher binds the socket and accepts connections; each worker
// dials in, writes "REGISTER <pid>\n", and then reads RECONCILE lines
// until disconnect.
//
// Messages:
//
//	REGISTER <pid>          worker → launcher  (sent once on dial)
//	RECONCILE               launcher → worker  (sent on each trigger)
//	BYE                     either-way; closes the connection cleanly
package sync

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	gosync "sync"
	"time"
)

const (
	MsgRegister  = "REGISTER"
	MsgReconcile = "RECONCILE"
	MsgBye       = "BYE"
)

// Server is the launcher side. Workers connect to it; the launcher
// calls Trigger to broadcast reconcile messages.
type Server struct {
	ln net.Listener

	mu      gosync.Mutex
	clients map[net.Conn]int // conn → registered pid
}

// NewServer creates and listens on the socket path.
//
// If a stale socket exists at path it is removed.
func NewServer(path string) (*Server, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	s := &Server{
		ln:      ln,
		clients: map[net.Conn]int{},
	}
	go s.acceptLoop()
	return s, nil
}

func (s *Server) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.serveClient(c)
	}
}

func (s *Server) serveClient(c net.Conn) {
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, MsgRegister+" ") {
		_ = c.Close()
		return
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(line, MsgRegister+" "))
	if err != nil {
		_ = c.Close()
		return
	}
	s.mu.Lock()
	s.clients[c] = pid
	s.mu.Unlock()

	// Drain any stray messages (BYE) until the connection drops.
	go func() {
		for {
			_, err := r.ReadString('\n')
			if err != nil {
				s.mu.Lock()
				delete(s.clients, c)
				s.mu.Unlock()
				_ = c.Close()
				return
			}
		}
	}()
}

// Trigger broadcasts RECONCILE to all currently connected workers.
//
// Returns the number of workers reached. Slow or stalled clients are
// dropped after a short write deadline.
func (s *Server) Trigger() int {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.clients))
	for c := range s.clients {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	n := 0
	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		if _, err := c.Write([]byte(MsgReconcile + "\n")); err == nil {
			n++
		}
	}
	return n
}

// Close stops accepting new clients and closes existing connections.
func (s *Server) Close() error {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.clients))
	for c := range s.clients {
		conns = append(conns, c)
	}
	s.clients = nil
	s.mu.Unlock()
	for _, c := range conns {
		_, _ = c.Write([]byte(MsgBye + "\n"))
		_ = c.Close()
	}
	return s.ln.Close()
}

// Connect dials the server, registers, and returns a channel that
// fires once per RECONCILE message. The channel closes when ctx is
// cancelled or the server hangs up.
func Connect(ctx context.Context, path string) (<-chan struct{}, error) {
	d := net.Dialer{}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(c, "%s %d\n", MsgRegister, os.Getpid()); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("register: %w", err)
	}
	out := make(chan struct{}, 16)
	go func() {
		defer close(out)
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			switch line {
			case MsgReconcile:
				select {
				case out <- struct{}{}:
				default:
					// Drop if the consumer is slow; the harness
					// would rather miss a beat than queue.
				}
			case MsgBye:
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return out, nil
}
