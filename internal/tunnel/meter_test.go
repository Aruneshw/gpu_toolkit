package tunnel

import (
	"bytes"
	"io"
	"net"
	"testing"
)

// Request/response exchanges through MeteredPipe: every byte arrives, both
// directions are counted, and each request that follows an answer is a turn.
func TestMeteredPipeCountsBytesAndTurns(t *testing.T) {
	hostSide, local := net.Pipe()    // llama-server <-> host forward
	remote, workerSide := net.Pipe() // host tunnel <-> worker
	m := &Meter{}
	done := make(chan struct{})
	go func() { MeteredPipe(local, remote, m); close(done) }()
	go func() { // the worker answers each 4-byte request with 6 bytes
		buf := make([]byte, 4)
		for {
			if _, err := io.ReadFull(workerSide, buf); err != nil {
				workerSide.Close()
				return
			}
			workerSide.Write(append([]byte("ok"), buf...))
		}
	}()
	for i := 0; i < 5; i++ {
		if _, err := hostSide.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 6)
		if _, err := io.ReadFull(hostSide, got); err != nil || !bytes.Equal(got, []byte("okping")) {
			t.Fatalf("%q %v", got, err)
		}
	}
	hostSide.Close()
	<-done
	// The first request follows no answer; the other four are turns.
	if c := m.Counts(); c != (MeterCounts{Up: 20, Down: 30, Turns: 4}) {
		t.Fatalf("%+v", c)
	}
}

// A one-way upload (a tensor sent to the worker) moves every byte through
// the buffer and is not counted as turns.
func TestMeteredPipeUploadIsNoTurn(t *testing.T) {
	hostSide, local := net.Pipe()
	remote, workerSide := net.Pipe()
	m := &Meter{}
	done := make(chan struct{})
	go func() { MeteredPipe(local, remote, m); close(done) }()
	big := bytes.Repeat([]byte("tensor"), pipeBuffer)
	go func() { hostSide.Write(big); hostSide.Close() }()
	got, err := io.ReadAll(workerSide)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("upload damaged: %d bytes, %v", len(got), err)
	}
	<-done
	if c := m.Counts(); c.Up != uint64(len(big)) || c.Down != 0 || c.Turns != 0 {
		t.Fatalf("%+v", c)
	}
}
