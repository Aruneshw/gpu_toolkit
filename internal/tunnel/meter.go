package tunnel

import (
	"io"
	"sync/atomic"
)

// Meter counts the traffic of llama.cpp RPC streams to one worker: bytes
// each way and turns. A turn is the host sending again after the worker has
// answered, so for RPC's strict request/response exchanges it counts round
// trips, each of which costs one link round-trip time.
type Meter struct {
	up, down atomic.Uint64 // host -> worker, worker -> host
	turns    atomic.Uint64
	last     atomic.Int32 // 0 none, 1 up, 2 down
}

// MeterCounts is a snapshot of a Meter.
type MeterCounts struct {
	Up, Down, Turns uint64
}

// Counts returns the totals so far.
func (m *Meter) Counts() MeterCounts {
	return MeterCounts{Up: m.up.Load(), Down: m.down.Load(), Turns: m.turns.Load()}
}

type meteredWriter struct {
	w  io.Writer
	m  *Meter
	up bool
}

// Write records the direction before passing the data on: once an answer
// reaches llama-server it may send its next request at once, and that turn
// must see the answer already recorded.
func (c meteredWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return c.w.Write(p)
	}
	if c.up {
		if c.m.last.Swap(1) == 2 {
			c.m.turns.Add(1)
		}
	} else {
		c.m.last.Store(2)
	}
	n, err := c.w.Write(p)
	if n > 0 {
		if c.up {
			c.m.up.Add(uint64(n))
		} else {
			c.m.down.Add(uint64(n))
		}
	}
	return n, err
}

// pipeBuffer is the copy buffer per direction. TLS writes 16 KiB records, so
// larger buffers measured no faster; this keeps many open streams cheap.
const pipeBuffer = 64 << 10

// MeteredPipe is Pipe between the host side of an RPC stream (local) and the
// worker side (remote), counting into m when it is not nil.
func MeteredPipe(local, remote io.ReadWriteCloser, m *Meter) {
	var toRemote, toLocal io.Writer = remote, local
	if m != nil {
		toRemote, toLocal = meteredWriter{remote, m, true}, meteredWriter{local, m, false}
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.CopyBuffer(toRemote, onlyReader{local}, make([]byte, pipeBuffer))
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.CopyBuffer(toLocal, onlyReader{remote}, make([]byte, pipeBuffer))
		done <- struct{}{}
	}()
	<-done
	local.Close()
	remote.Close()
	<-done
}

// onlyReader hides WriterTo/ReaderFrom so CopyBuffer really uses the buffer
// (and every byte passes the meter).
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }
