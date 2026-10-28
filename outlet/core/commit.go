// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
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

// flushed commits every tracked position. It must only be called once the
// buffered flows have been written, otherwise the committed offsets would run
// ahead of the data.
func (t *commitTracker) flushed() {
	for pos, record := range t.pending {
		t.commit(record)
		delete(t.pending, pos)
	}
}
