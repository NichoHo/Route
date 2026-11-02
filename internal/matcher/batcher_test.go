package matcher

import (
	"testing"
	"time"

	"route/internal/config"
)

// The sweep re-triggers orders that are still waiting for a batch. An order
// must take one slot in the queue however often it is triggered, or repeats
// crowd real orders out of full batches.
func TestTriggerKeepsEachWaitingOrderOnce(t *testing.T) {
	old := config.BatchWindow
	config.BatchWindow = time.Hour // never flush: no database in this test
	defer func() { config.BatchWindow = old }()

	b := NewBatcher(nil, nil, nil, nil, nil, nil)
	defer b.Stop()

	for i := 0; i < 3; i++ {
		b.Trigger("a")
		b.Trigger("b")
	}
	b.waitMu.Lock()
	n := len(b.waiting)
	b.waitMu.Unlock()
	if n != 2 {
		t.Fatalf("waiting = %d ids, want 2", n)
	}

	// Once an order's batch has been flushed, a trigger is a new request.
	b.stopWaiting([]string{"a"})
	b.Trigger("a")
	b.waitMu.Lock()
	_, again := b.waiting["a"]
	b.waitMu.Unlock()
	if !again {
		t.Fatal("a trigger after the batch was flushed was ignored")
	}
}
