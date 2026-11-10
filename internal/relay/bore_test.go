package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBore implements the server side of the bore protocol for tests.
type fakeBore struct {
	t       *testing.T
	control net.Listener
	mu      sync.Mutex
	taken   map[int]bool
	pending map[string]chan net.Conn
	public  map[int]net.Listener
	n       int
	drop    chan struct{} // closing a control connection simulates a relay drop
}

func newFakeBore(t *testing.T) *fakeBore {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBore{t: t, control: l, taken: map[int]bool{}, pending: map[string]chan net.Conn{}, public: map[int]net.Listener{}, drop: make(chan struct{}, 4)}
	go f.serve()
	t.Cleanup(func() {
		l.Close()
		f.mu.Lock()
		for _, p := range f.public {
			p.Close()
		}
		f.mu.Unlock()
	})
	return f
}

// dial routes the relay's control port to the fake server.
func (f *fakeBore) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if strings.HasSuffix(addr, ":7835") {
		addr = f.control.Addr().String()
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func frame(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	_, _ = c.Write(append(b, 0))
}

func (f *fakeBore) serve() {
	for {
		c, err := f.control.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeBore) handle(c net.Conn) {
	r := bufio.NewReader(c)
	b, err := r.ReadBytes(0)
	if err != nil {
		c.Close()
		return
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(b[:len(b)-1], &msg) != nil {
		c.Close()
		return
	}
	if raw, ok := msg["Accept"]; ok {
		var id string
		_ = json.Unmarshal(raw, &id)
		f.mu.Lock()
		ch := f.pending[id]
		f.mu.Unlock()
		if ch != nil {
			ch <- c
		} else {
			c.Close()
		}
		return
	}
	var port int
	_ = json.Unmarshal(msg["Hello"], &port)
	f.mu.Lock()
	if port != 0 && f.taken[port] {
		f.mu.Unlock()
		frame(c, map[string]string{"Error": "port already in use"})
		c.Close()
		return
	}
	pl, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		f.mu.Unlock()
		frame(c, map[string]string{"Error": "port already in use"})
		c.Close()
		return
	}
	port = pl.Addr().(*net.TCPAddr).Port
	f.taken[port] = true
	f.public[port] = pl
	f.mu.Unlock()
	frame(c, map[string]int{"Hello": port})
	stop := make(chan struct{})
	release := func() {
		f.mu.Lock()
		delete(f.taken, port)
		delete(f.public, port)
		f.mu.Unlock()
		pl.Close()
		c.Close()
	}
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-f.drop:
				release()
				return
			case <-t.C:
				frame(c, "Heartbeat")
			}
		}
	}()
	defer close(stop)
	for {
		visitor, err := pl.Accept()
		if err != nil {
			release()
			return
		}
		f.mu.Lock()
		f.n++
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.n)
		ch := make(chan net.Conn, 1)
		f.pending[id] = ch
		f.mu.Unlock()
		frame(c, map[string]string{"Connection": id})
		go func() {
			select {
			case back := <-ch:
				go func() { _, _ = io.Copy(back, visitor); back.Close() }()
				_, _ = io.Copy(visitor, back)
				visitor.Close()
			case <-time.After(5 * time.Second):
				visitor.Close()
			}
		}()
	}
}

func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().String()
}

func roundTrip(t *testing.T, port int, text string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(text))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != text {
		t.Fatalf("relay round trip: %q %v", got, err)
	}
}

func TestTunnelForwardsVisitors(t *testing.T) {
	f := newFakeBore(t)
	tun := &Tunnel{Server: "relay.test", Local: echoServer(t), Dial: f.dial}
	s, err := tun.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); roundTrip(t, s.Port, fmt.Sprintf("hello-%d through the relay", i)) }(i)
	}
	wg.Wait()
	// The same port cannot be claimed twice.
	other := &Tunnel{Server: "relay.test", Port: s.Port, Local: "127.0.0.1:1", Dial: f.dial}
	if _, err := other.Open(context.Background()); err != ErrPortInUse {
		t.Fatalf("expected ErrPortInUse, got %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestKeepReclaimsSamePortAfterDrop(t *testing.T) {
	f := newFakeBore(t)
	tun := &Tunnel{Server: "relay.test", Local: echoServer(t), Dial: f.dial}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ports := make(chan int, 4)
	go func() { _ = tun.Keep(ctx, func(p int) { ports <- p }) }()
	first := <-ports
	roundTrip(t, first, "before drop")
	f.drop <- struct{}{}
	select {
	case again := <-ports:
		if again != first {
			t.Fatalf("reconnected on port %d, want %d", again, first)
		}
		roundTrip(t, again, "after drop")
	case <-time.After(15 * time.Second):
		t.Fatal("tunnel did not reconnect")
	}
}

func TestRecvParsesFrames(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\"Heartbeat\"\x00{\"Hello\":4242}\x00{\"Connection\":\"abc\"}\x00{\"Error\":\"nope\"}\x00"))
	want := []message{{kind: "Heartbeat"}, {kind: "Hello", port: 4242}, {kind: "Connection", value: "abc"}, {kind: "Error", value: "nope"}}
	for _, w := range want {
		m, err := recv(r)
		if err != nil || m != w {
			t.Fatalf("got %+v %v want %+v", m, err, w)
		}
	}
	big := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 600)+"\x00"), maxFrame)
	if _, err := recv(big); err == nil {
		t.Fatal("accepted oversized frame")
	}
}
