// SPDX-FileCopyrightText: 2024 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package kafkainput

import (
	"context"
	"errors"
	"strconv"

	"github.com/rs/zerolog"
	"github.com/twmb/franz-go/pkg/kgo"

	"akvorado/common/reporter"
)

// ErrStopProcessing should be returned as an error when we need to stop processing more flows.
var ErrStopProcessing = errors.New("stop processing further flows")

// Consumer is a franz-go consumer and should process flow messages.
type Consumer struct {
	r *reporter.Reporter
	l zerolog.Logger

	metrics  metrics
	worker   int
	callback ReceiveFunc
	client   *kgo.Client
}

// Record is a message received from Kafka.
type Record struct {
	Topic     string
	Partition int32
	Epoch     int32
	Offset    int64
	Value     []byte
}

// ReceiveFunc is a function that will be called with each received message.
type ReceiveFunc func(context.Context, Record) error

// ShutdownFunc is a function that will be called on shutdown of the consumer.
type ShutdownFunc func()

// RevokeFunc is a function called when partitions are revoked, before they are
// handed to another member. Implementations must forget the positions tracked
// for these partitions: committing them later would move the group's committed
// offset backwards.
type RevokeFunc func(map[string][]int32)

// CommitFunc is a function to call once the flows of a record are durable.
// The consumer only commits the offsets of the records reported through this
// function: a record still sitting in an in-memory batch must not be committed,
// otherwise a crash or a failed flush loses it forever.
type CommitFunc func(Record)

// WorkerBuilderFunc returns a function to be called with each received messages,
// a function to be called when shutting down, and a function to be called when
// partitions are revoked. It is provided the worker number (for logging
// purpose), a chan for the worker to request more or less workers, and the
// function to call once a record has been flushed.
type WorkerBuilderFunc func(int, chan<- ScaleRequest, CommitFunc) (ReceiveFunc, ShutdownFunc, RevokeFunc)

// NewConsumer creates a new consumer.
func (c *realComponent) newConsumer(worker int, callback ReceiveFunc) *Consumer {
	return &Consumer{
		r: c.r,
		l: c.r.With().Int("worker", worker).Logger(),

		worker:   worker,
		metrics:  c.metrics,
		callback: callback,
	}
}

// MarkFlushed records that the flows of the given record have been written to
// their destination, so its offset can be committed.
func (c *Consumer) MarkFlushed(record Record) {
	if c.client == nil {
		return
	}
	c.client.MarkCommitOffsets(map[string]map[int32]kgo.EpochOffset{
		record.Topic: {
			record.Partition: kgo.EpochOffset{Epoch: record.Epoch, Offset: record.Offset + 1},
		},
	})
}

// ProcessFetches processes the fetched records.
func (c *Consumer) ProcessFetches(ctx context.Context, client *kgo.Client, fetches kgo.Fetches) error {
	if fetches.Empty() {
		return nil
	}

	// Keep the client around: offsets are marked from MarkFlushed(), possibly
	// long after this call returned.
	c.client = client

	worker := strconv.Itoa(c.worker)
	c.metrics.fetchesReceived.WithLabelValues(worker).Inc()

	if errs := fetches.Errors(); len(errs) > 0 {
		for _, err := range errs {
			if errors.Is(err.Err, context.Canceled) || errors.Is(err.Err, ErrStopProcessing) {
				return nil
			}
			c.metrics.errorsReceived.WithLabelValues(worker).Inc()
			c.l.Err(err.Err).
				Str("topic", err.Topic).
				Int32("partition", err.Partition).
				Msg("fetch error")
		}
		// Assume the error is fatal. The data is uncommitted, so it should be OK.
		return ErrStopProcessing
	}

	messagesReceived := c.metrics.messagesReceived.WithLabelValues(worker)
	bytesReceived := c.metrics.bytesReceived.WithLabelValues(worker)
	for _, fetch := range fetches {
		for _, topic := range fetch.Topics {
			for _, partition := range topic.Partitions {
				for _, record := range partition.Records {
					if err := ctx.Err(); err != nil {
						return err
					}
					messagesReceived.Inc()
					bytesReceived.Add(float64(len(record.Value)))
					if err := c.callback(ctx, Record{
						Topic:     topic.Topic,
						Partition: partition.Partition,
						Epoch:     record.LeaderEpoch,
						Offset:    record.Offset,
						Value:     record.Value,
					}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
