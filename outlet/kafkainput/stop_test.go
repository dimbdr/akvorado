// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package kafkainput

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestStopWorkersInParallel checks every worker is stopped concurrently. Each
// worker may wait for the ClickHouse grace period when flushing its last batch:
// stopping them one after the other would exceed the pod's termination grace
// period and get the process SIGKILLed before the last batches are written.
func TestStopWorkersInParallel(t *testing.T) {
	c := &realComponent{}
	const workers = 4
	var stopping atomic.Int32
	release := make(chan struct{})
	for range workers {
		c.workers = append(c.workers, worker{stop: func() {
			stopping.Add(1)
			<-release
		}})
	}

	done := make(chan struct{})
	go func() {
		c.StopWorkers()
		close(done)
	}()

	// All workers must be stopping at the same time.
	deadline := time.After(time.Second)
	for stopping.Load() != workers {
		select {
		case <-deadline:
			t.Fatalf("only %d/%d workers stopped concurrently", stopping.Load(), workers)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StopWorkers() did not return")
	}
}
