// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package templatecache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"akvorado/common/kafka"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Start starts the cache. It is best-effort: a Kafka error leaves the cache
// usable as a local cache and never blocks the outlet.
func (c *Cache) Start() error {
	c.started = true
	c.t.Go(func() error {
		<-c.t.Dying()
		return nil
	})
	if !c.config.Enabled {
		c.startPurge()
		return nil
	}

	producerOpts, err := kafka.NewConfig(c.r, c.config.Configuration)
	if err != nil {
		c.r.Err(err).Msg("cannot configure template cache Kafka client")
		c.startPurge()
		return nil
	}
	producerOpts = append(producerOpts, kgo.MaxBufferedRecords(c.config.QueueSize))
	producer, err := kgo.NewClient(producerOpts...)
	if err != nil {
		c.r.Err(err).Msg("cannot create template cache Kafka producer")
		c.startPurge()
		return nil
	}
	c.producer = producer

	consumerOpts, err := kafka.NewConfig(c.r, c.config.Configuration)
	if err != nil {
		c.r.Err(err).Msg("cannot configure template cache Kafka consumer")
		producer.Close()
		c.producer = nil
		c.startPurge()
		return nil
	}
	consumerOpts = append(consumerOpts,
		kgo.ConsumeTopics(c.topic),
		kgo.ConsumeStartOffset(kgo.NewOffset().AtStart()),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(100*time.Millisecond),
	)
	consumer, err := kgo.NewClient(consumerOpts...)
	if err != nil {
		c.r.Err(err).Msg("cannot create template cache Kafka consumer")
		producer.Close()
		c.producer = nil
		c.startPurge()
		return nil
	}
	c.consumer = consumer

	ctx, cancel := context.WithTimeout(context.Background(), c.config.PreloadTimeout)
	c.preload(ctx)
	cancel()

	c.t.Go(func() error {
		c.tail(c.t.Context(context.Background()))
		return nil
	})
	c.startPurge()
	return nil
}

// Stop stops the cache.
func (c *Cache) Stop() error {
	if !c.started {
		return nil
	}
	c.t.Kill(nil)
	c.t.Wait()
	if c.consumer != nil {
		c.consumer.Close()
		c.consumer = nil
	}
	if c.producer != nil {
		c.producer.Close()
		c.producer = nil
	}
	return nil
}

// startPurge starts the TTL purge goroutine when a TTL is configured.
func (c *Cache) startPurge() {
	if c.config.TTL <= 0 {
		return
	}
	interval := c.config.TTL / 2
	if interval < time.Second {
		interval = time.Second
	}
	c.t.Go(func() error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.t.Dying():
				return nil
			case <-ticker.C:
				c.purge()
			}
		}
	})
}

// preload reads the topic from its start offset up to its end offset.
func (c *Cache) preload(ctx context.Context) {
	admin := kadm.NewClient(c.consumer)
	starts, err := admin.ListStartOffsets(ctx, c.topic)
	if err != nil {
		if errors.Is(err, kerr.UnknownTopicOrPartition) {
			c.r.Debug().Str("topic", c.topic).Msg("template cache topic does not exist yet")
			return
		}
		c.r.Warn().Err(err).Msg("cannot list template cache start offsets")
		return
	}
	ends, err := admin.ListEndOffsets(ctx, c.topic)
	if err != nil {
		if errors.Is(err, kerr.UnknownTopicOrPartition) {
			c.r.Debug().Str("topic", c.topic).Msg("template cache topic does not exist yet")
			return
		}
		c.r.Warn().Err(err).Msg("cannot list template cache end offsets")
		return
	}
	if hasUnknownTopic(starts) || hasUnknownTopic(ends) {
		c.r.Debug().Str("topic", c.topic).Msg("template cache topic does not exist yet")
		return
	}

	type partition struct {
		topic string
		id    int32
	}
	next := make(map[partition]int64)
	starts.Each(func(offset kadm.ListedOffset) {
		next[partition{offset.Topic, offset.Partition}] = offset.Offset
	})
	done := func() bool {
		finished := true
		ends.Each(func(offset kadm.ListedOffset) {
			if offset.Err != nil {
				return
			}
			if next[partition{offset.Topic, offset.Partition}] < offset.Offset {
				finished = false
			}
		})
		return finished
	}
	if done() {
		return
	}

	for !done() {
		fetches := c.consumer.PollFetches(ctx)
		if ctx.Err() != nil {
			c.r.Warn().Err(ctx.Err()).Str("topic", c.topic).Msg("template cache preload timed out")
			return
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, fetchErr := range errs {
				if errors.Is(fetchErr.Err, context.Canceled) {
					return
				}
				c.r.Debug().
					Err(fetchErr.Err).
					Str("topic", fetchErr.Topic).
					Int32("partition", fetchErr.Partition).
					Msg("template cache preload fetch error")
			}
		}
		fetches.EachRecord(func(record *kgo.Record) {
			c.applyRecord(record)
			next[partition{record.Topic, record.Partition}] = record.Offset + 1
		})
	}
}

// tail applies records as they arrive on the topic.
func (c *Cache) tail(ctx context.Context) {
	for {
		fetches := c.consumer.PollFetches(ctx)
		if ctx.Err() != nil {
			return
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, fetchErr := range errs {
				if errors.Is(fetchErr.Err, context.Canceled) {
					return
				}
				c.r.Debug().
					Err(fetchErr.Err).
					Str("topic", fetchErr.Topic).
					Int32("partition", fetchErr.Partition).
					Msg("template cache fetch error")
			}
		}
		fetches.EachRecord(c.applyRecord)
	}
}

// applyRecord applies a record read from the topic.
func (c *Cache) applyRecord(record *kgo.Record) {
	c.metrics.received.Inc()
	var key Key
	if err := key.UnmarshalText(record.Key); err != nil {
		c.metrics.invalidRecords.Inc()
		c.r.Debug().Err(err).Msg("invalid template cache record key")
		return
	}
	if record.Value == nil {
		var origin string
		for _, header := range record.Headers {
			if header.Key == "origin" {
				origin = string(header.Value)
				break
			}
		}
		c.deleteFromKafka(key, origin, record.Timestamp)
		return
	}
	var entry Entry
	if err := json.Unmarshal(record.Value, &entry); err != nil {
		c.metrics.invalidRecords.Inc()
		c.r.Debug().Err(err).Msg("invalid template cache record")
		return
	}
	c.set(key, entry, sourceKafka)
}

// deleteFromKafka applies a tombstone read from the topic.
func (c *Cache) deleteFromKafka(key Key, origin string, timestamp time.Time) {
	c.mu.Lock()
	stored, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	if origin == c.origin && stored.Origin == c.origin && !stored.UpdatedAt.Before(timestamp) {
		c.mu.Unlock()
		return
	}
	delete(c.entries, key)
	delete(c.lastWrite, key)
	c.metrics.entries.Set(float64(len(c.entries)))
	c.mu.Unlock()
}

// hasUnknownTopic tells if the listed offsets report a missing topic.
func hasUnknownTopic(offsets kadm.ListedOffsets) bool {
	found := false
	offsets.Each(func(offset kadm.ListedOffset) {
		if errors.Is(offset.Err, kerr.UnknownTopicOrPartition) {
			found = true
		}
	})
	return found
}
