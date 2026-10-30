// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package kafkainput

import (
	"context"
	"errors"
	"fmt"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kprom"
)

// worker represents a worker
type worker struct {
	stop func()
}

// newClient returns a new Kafka client
func (c *realComponent) newClient(i int, revoke RevokeFunc) (*kgo.Client, error) {
	logger := c.r.With().Int("worker", i).Logger()
	logger.Info().Msg("starting new client")

	kmetrics := kprom.NewMetrics("", kprom.WithStaticLabel(prometheus.Labels{"worker": strconv.Itoa(i)}))
	kafkaOpts := append([]kgo.Opt{}, c.kafkaOpts...)
	kafkaOpts = append(kafkaOpts,
		kgo.WithHooks(kmetrics),
		// kgo replaces the callback instead of chaining them, so the
		// component-level callback cannot be used: each client commits its
		// marked offsets and then tells its own worker to forget the revoked
		// partitions.
		kgo.OnPartitionsRevoked(onPartitionsRevoked(logger, revoke)))

	client, err := kgo.NewClient(kafkaOpts...)
	if err != nil {
		logger.Err(err).
			Int("worker", i).
			Str("brokers", strings.Join(c.config.Brokers, ",")).
			Msg("unable to create new client")
		return nil, fmt.Errorf("unable to create Kafka client: %w", err)
	}

	c.kafkaMetrics = append(c.kafkaMetrics, kmetrics)
	c.r.RegisterMetricCollector(kmetrics)
	return client, nil
}

// onPartitionsRevoked commits the offsets already marked, then tells the worker
// to forget the revoked partitions. Committing them later would move the
// group's committed offset backwards once another member owns them.
func onPartitionsRevoked(logger zerolog.Logger, revoke RevokeFunc) func(context.Context, *kgo.Client, map[string][]int32) {
	return func(ctx context.Context, client *kgo.Client, revoked map[string][]int32) {
		if err := client.CommitMarkedOffsets(ctx); err != nil {
			logger.Err(err).Msg("cannot commit marked offsets on partition revoked")
		}
		if revoke != nil {
			revoke(revoked)
		}
	}
}

// startOneWorker starts a new worker.
func (c *realComponent) startOneWorker() error {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()

	// New consumer
	i := len(c.workers)
	consumer := c.newConsumer(i, nil)
	callback, shutdown, revoke := c.workerBuilder(i, c.workerRequestChan, consumer.MarkFlushed)
	consumer.callback = callback
	client, err := c.newClient(i, revoke)
	if err != nil {
		return err
	}

	// Goroutine for worker
	done := make(chan bool)
	ctx, cancel := context.WithCancelCause(context.Background())
	ctx = c.t.Context(ctx)
	c.t.Go(func() error {
		logger := c.r.With().
			Int("worker", i).
			Logger()
		labels := pprof.Labels("goroutine", fmt.Sprintf("kafka-worker-%d", i))
		pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), labels))
		defer func() {
			logger.Info().Msg("stopping worker")
			if err := stopWorker(shutdown, client.CommitMarkedOffsets); err != nil {
				logger.Err(err).Msg("cannot commit marked partition offsets")
			}
			client.CloseAllowingRebalance()

			close(done)
		}()

		for {
			select {
			case <-ctx.Done():
				return nil
			default:
				fetches := client.PollFetches(ctx)
				if fetches.IsClientClosed() {
					logger.Error().Msg("client is closed")
					return errors.New("client is closed")
				}
				if err := consumer.ProcessFetches(ctx, client, fetches); err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, ErrStopProcessing) {
						return nil
					}
					logger.Err(err).Msg("cannot process fetched messages")
					return fmt.Errorf("cannot process fetched messages: %w", err)
				}
				client.AllowRebalance()
			}
		}
	})

	c.workers = append(c.workers, worker{
		stop: func() {
			cancel(ErrStopProcessing)
			<-done
		},
	})
	c.metrics.workerIncrease.Inc()
	return nil
}

// stopWorker flushes the worker's last batch, then commits the offsets the
// flush marked. The order matters: committing before the flush would persist
// the position of flows still buffered in memory, making them unrecoverable if
// the flush fails.
func stopWorker(shutdown ShutdownFunc, commit func(context.Context) error) error {
	shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return commit(ctx)
}

// stopOneWorker stops the last worker
func (c *realComponent) stopOneWorker() {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()
	i := len(c.workers) - 1
	worker := c.workers[i]
	worker.stop()
	c.workers = c.workers[:i]

	kmetrics := c.kafkaMetrics[i]
	c.r.UnregisterMetricCollector(kmetrics)
	c.kafkaMetrics = c.kafkaMetrics[:i]

	c.metrics.workerDecrease.Inc()
}
