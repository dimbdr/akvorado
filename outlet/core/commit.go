// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"sync"

	"akvorado/outlet/kafkainput"
)

// position identifies a partition of a topic.
type position struct {
	topic     string
	partition int32
}

// commitTracker remembers the latest record seen on each partition and commits
// it once its flows are durable. Committing only after a successful flush keeps
// the outlet at-least-once: on a crash or a failed flush, the flows are
// re-consumed instead of being lost forever.
type commitTracker struct {
	mu      sync.Mutex
	commit  kafkainput.CommitFunc
	pending map[position]kafkainput.Record
}

func newCommitTracker(commit kafkainput.CommitFunc) *commitTracker {
	return &commitTracker{
		commit:  commit,
		pending: make(map[position]kafkainput.Record),
	}
}

// track records the position of a record whose flows have just been buffered.
func (t *commitTracker) track(record kafkainput.Record) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pos := position{topic: record.Topic, partition: record.Partition}
	if current, ok := t.pending[pos]; !ok || record.Offset > current.Offset {
		// Do not retain the payload: only the position matters.
		t.pending[pos] = kafkainput.Record{
			Topic:     record.Topic,
			Partition: record.Partition,
			Epoch:     record.Epoch,
			Offset:    record.Offset,
		}
	}
}

// forget drops every tracked position of the given partitions. It is called
// when they are revoked: another member now owns them, and committing our stale
// position would move the group's committed offset backwards.
func (t *commitTracker) forget(revoked map[string][]int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for topic, partitions := range revoked {
		for _, partition := range partitions {
			delete(t.pending, position{topic: topic, partition: partition})
		}
	}
}

// flushed commits every tracked position. It must only be called once the
// buffered flows have been written, otherwise the committed offsets would run
// ahead of the data.
func (t *commitTracker) flushed() {
	t.mu.Lock()
	records := make([]kafkainput.Record, 0, len(t.pending))
	for pos, record := range t.pending {
		records = append(records, record)
		delete(t.pending, pos)
	}
	t.mu.Unlock()
	// Commit without holding the lock: the consumer takes its own lock and a
	// revoke callback may commit concurrently.
	for _, record := range records {
		t.commit(record)
	}
}
