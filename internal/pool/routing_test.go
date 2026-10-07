package pool

import (
	"context"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func TestRoutingQueueBoundedAndDrainedWithExpiryOff(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	e := store.RoutingEvent{TS: time.Now().Unix(), Policy: "rebalance", Mode: "live", Kind: "pending", Reason: "usage-advantage"}
	for range cap(p.routingEvents) + 1 {
		p.recordRouting(e)
	}
	if p.routingDropped.Load() != 1 {
		t.Fatal("overflow was not counted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.RoutingLoop(ctx, false) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM routing_event`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 257 {
			var shadow int
			if err := db.QueryRow(`SELECT COUNT(*) FROM routing_event WHERE mode='shadow'`).Scan(&shadow); err != nil || shadow != 0 {
				t.Fatalf("off mode wrote shadow: %d %v", shadow, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("routing queue was not drained")
}
