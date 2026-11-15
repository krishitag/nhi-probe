package proxy

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/krishitag/nhi-probe/internal/event"
)

// sink is the event writer: a bounded queue plus ONE goroutine that writes to the file.
// Only one goroutine touches the file, so no lock is needed for it.
type sink struct {
	ch   chan event.Event // bounded: when full, send() waits (backpressure)
	quit chan struct{}    // closed by stop(): "write what's left, then finish"
	done chan struct{}    // closed by run() when it has finished
	w    io.Writer
}

func newSink(w io.Writer, size int) *sink {
	return &sink{
		ch:   make(chan event.Event, size),
		quit: make(chan struct{}),
		done: make(chan struct{}),
		w:    w,
	}
}

// run is the writer goroutine. Start it with: go s.run()
func (s *sink) run() {
	defer close(s.done)
	for {
		select {
		case e := <-s.ch:
			s.write(e)
		case <-s.quit:
			// Drain: write everything still waiting in the queue, then stop.
			for {
				select {
				case e := <-s.ch:
					s.write(e)
				default:
					return
				}
			}
		}
	}
}

// send puts an event in the queue. If the queue is full it WAITS (backpressure:
// the proxy reads the agent more slowly). It gives up only if ctx is cancelled.
// We never close s.ch, so a late send can never panic.
func (s *sink) send(ctx context.Context, e event.Event) bool {
	select {
	case s.ch <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// stop tells the writer to drain and finish, and waits until it has.
func (s *sink) stop() {
	close(s.quit)
	<-s.done
}

// write saves one event. Each event is one Write call, so the file is valid JSONL
// even if we are killed right after.
func (s *sink) write(e event.Event) {
	if err := writeEvent(s.w, e); err != nil {
		fmt.Fprintln(os.Stderr, "nhi-probe:", err)
	}
}
