package main

import (
	"testing"
	"time"
)

// The spec bounds Runtime.Wait at 10 s. whatsmeow ignores the context handed
// to Connect, so an actor stuck in a dial is not cancellable and an unbounded
// wait would hand the shutdown to SIGKILL.
func TestWaitBoundedReportsTimeout(t *testing.T) {
	done := make(chan struct{})
	if waitBounded(func() { <-done }, 20*time.Millisecond) {
		t.Fatal("waitBounded reported a clean finish for a wait that never returned")
	}
	close(done)
	if !waitBounded(func() {}, time.Second) {
		t.Fatal("waitBounded reported a timeout for a wait that returned immediately")
	}
}
