package tunnel

import (
	"context"
	"errors"
	"net"
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
