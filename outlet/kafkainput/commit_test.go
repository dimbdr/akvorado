// SPDX-FileCopyrightText: 2026 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package kafkainput

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"akvorado/common/daemon"
	"akvorado/common/helpers"
	"akvorado/common/kafka"
	"akvorado/common/pb"
	"akvorado/common/reporter"
)

// committedAtLeast returns a kfake.GroupInfo condition checking that the group
// committed at least the given offset on the topic.
func committedAtLeast(topic string, want int64) func(*kfake.GroupInfo) bool {
	return func(g *kfake.GroupInfo) bool {
		if g == nil {
			return false
		}
		var committed int64
		for _, commit := range g.Commits[topic] {
			committed += commit.Offset
		}
		return committed >= want
	}
}

// TestOffsetsCommittedOnlyAfterFlush checks the consumer never commits an
// offset before the worker reports the corresponding flows as flushed: a
// record still sitting in the ClickHouse batch buffer must not be committed,
// otherwise a crash or a failed final flush loses it forever.
func TestOffsetsCommittedOnlyAfterFlush(t *testing.T) {
	r := reporter.NewMock(t)
	topicName := fmt.Sprintf("test-topic-commit-%d", rand.Int())
	expectedTopicName := fmt.Sprintf("%s-v%d", topicName, pb.Version)

	cluster, err := kfake.NewCluster(
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, expectedTopicName),
		kfake.WithLogger(kafka.NewLogger(r)),
	)
	if err != nil {
		t.Fatalf("NewCluster() error: %v", err)
	}
	defer cluster.Close()

	// Create a producer client.
	producerConfiguration := kafka.DefaultConfiguration()
	producerConfiguration.Brokers = cluster.ListenAddrs()
	producerOpts, err := kafka.NewConfig(reporter.NewMock(t), producerConfiguration)
	if err != nil {
		t.Fatalf("NewConfig() error:\n%+v", err)
	}
	producerOpts = append(producerOpts, kgo.ProducerLinger(0))
	producer, err := kgo.NewClient(producerOpts...)
	if err != nil {
		t.Fatalf("NewClient() error:\n%+v", err)
	}
	defer producer.Close()

	// Start the component.
	configuration := DefaultConfiguration()
	configuration.Topic = topicName
	configuration.Brokers = cluster.ListenAddrs()
	configuration.FetchMaxWaitTime = 10 * time.Millisecond
	configuration.ConsumerGroup = fmt.Sprintf("outlet-%d", rand.Int())
	c, err := New(r, configuration, Dependencies{Daemon: daemon.NewMock(t)})
	if err != nil {
		t.Fatalf("New() error:\n%+v", err)
	}
	helpers.StartStop(t, c)

	// Wait for the consumer to fetch before producing, otherwise the record
	// would be before the initial offset and skipped.
	fetchFault := cluster.Fault(kfake.Fault{
		Keys:    []kmsg.Key{kmsg.Fetch},
		Observe: true,
		Count:   -1,
	})
	received := make(chan Record, 1)
	var commit CommitFunc
	c.StartWorkers(func(_ int, _ chan<- ScaleRequest, commitFn CommitFunc) (ReceiveFunc, ShutdownFunc) {
		commit = commitFn
		return func(_ context.Context, record Record) error {
			received <- record
			return nil
		}, func() {}
	})
	fetchCtx, fetchCancel := context.WithTimeout(t.Context(), time.Second)
	defer fetchCancel()
	if err := fetchFault.Wait(fetchCtx, 1); err != nil {
		t.Fatalf("Wait() error:\n%+v", err)
	}
	fetchFault.Remove()

	produced := &kgo.Record{
		Topic: expectedTopicName,
		Value: []byte("hello"),
	}
	if results := producer.ProduceSync(t.Context(), produced); results.FirstErr() != nil {
		t.Fatalf("ProduceSync() error:\n%+v", results.FirstErr())
	}
	var record Record
	select {
	case record = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not receive the message")
	}

	// The worker returned without reporting the record as flushed: the offset
	// must stay uncommitted, even after several auto-commit intervals.
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	if _, err := cluster.WaitGroupInfo(ctx, configuration.ConsumerGroup,
		committedAtLeast(expectedTopicName, 1)); err == nil {
		t.Fatal("offset was committed while the record was not flushed")
	}

	// Once the worker reports the record as flushed, the offset is committed.
	commit(record)
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cluster.WaitGroupInfo(ctx, configuration.ConsumerGroup,
		committedAtLeast(expectedTopicName, 1)); err != nil {
		t.Fatalf("offset was not committed after the record was flushed:\n%+v", err)
	}
}
