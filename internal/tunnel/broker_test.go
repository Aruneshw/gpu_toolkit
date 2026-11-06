package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestBrokerLeaseDisconnectAndReplacement(t *testing.T) {
	b, err := NewBroker(1)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a, remote := net.Pipe()
	defer remote.Close()
	if err := b.Register("worker-a", a); err != nil {
		t.Fatal(err)
	}
	extra, extraRemote := net.Pipe()
	defer extra.Close()
	defer extraRemote.Close()
	if err := b.Register("worker-b", extra); !errors.Is(err, ErrBrokerFull) {
		t.Fatalf("capacity: %v", err)
	}
	conn, release, err := b.Acquire(context.Background(), "worker-a")
	if err != nil || conn != a {
		t.Fatalf("acquire: %v", err)
	}
	b.Disconnect("worker-a")
	if _, err := remote.Write([]byte("x")); err == nil {
		t.Fatal("disconnect did not close leased connection")
	}
	newConn, newRemote := net.Pipe()
	defer newRemote.Close()
	if err := b.Register("worker-a", newConn); err != nil {
		t.Fatal(err)
	}
	// Releasing an older lease must not remove the replacement.
	release()
	release()
	got, done, err := b.Acquire(context.Background(), "worker-a")
	if err != nil || got != newConn {
		t.Fatalf("replacement lost: %v", err)
	}
	done()
}

func TestBrokerWaitCancellationAndClose(t *testing.T) {
	b, _ := NewBroker(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := b.Acquire(ctx, "worker"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := b.Acquire(context.Background(), "worker")
		result <- err
	}()
	_ = b.Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrBrokerClosed) {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter not woken")
	}
}

func TestBrokerWaitForWorker(t *testing.T) {
	b, _ := NewBroker(1)
	defer b.Close()
	a, remote := net.Pipe()
	defer remote.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, release, err := b.Acquire(ctx, "worker")
		if err == nil {
			if conn != a {
				t.Error("wrong worker connection")
			}
			release()
		}
		result <- err
	}()
	if err := b.Register("worker", a); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestBrokerConcurrentWorkers(t *testing.T) {
	const workers = 64
	b, err := NewBroker(workers)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("worker-%d", i)
			local, remote := net.Pipe()
			defer remote.Close()
			if err := b.Register(id, local); err != nil {
				local.Close()
				t.Errorf("register %s: %v", id, err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, release, err := b.Acquire(ctx, id)
			if err != nil {
				t.Errorf("acquire %s: %v", id, err)
				return
			}
			defer release()
			received := make(chan error, 1)
			go func() {
				var payload [1]byte
				_, err := remote.Read(payload[:])
				if err == nil && payload[0] != byte(i) {
					err = errors.New("data crossed worker identities")
				}
				received <- err
			}()
			if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Errorf("deadline: %v", err)
				return
			}
			if _, err := conn.Write([]byte{byte(i)}); err != nil {
				t.Errorf("write %s: %v", id, err)
				return
			}
			if err := <-received; err != nil {
				t.Errorf("read %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestBrokerExclusiveLease(t *testing.T) {
	b, _ := NewBroker(1)
	defer b.Close()
	local, remote := net.Pipe()
	defer remote.Close()
	if err := b.Register("worker", local); err != nil {
		t.Fatal(err)
	}
	_, release, err := b.Acquire(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	const contenders = 16
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, done, err := b.Acquire(ctx, "worker")
			if err == nil {
				done()
				t.Error("same connection leased twice")
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestBrokerConcurrentShutdown(t *testing.T) {
	const workers = 32
	b, _ := NewBroker(workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		id := fmt.Sprintf("worker-%d", i)
		local, remote := net.Pipe()
		defer remote.Close()
		if err := b.Register(id, local); err != nil {
			t.Fatal(err)
		}
		_, release, err := b.Acquire(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(2)
		go func() { defer wg.Done(); release() }()
		go func() { defer wg.Done(); b.Disconnect(id) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); b.Close() }()
	wg.Wait()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}
