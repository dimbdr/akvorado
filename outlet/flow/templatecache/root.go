// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package templatecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"akvorado/common/reporter"

	"github.com/twmb/franz-go/pkg/kgo"
)

// source describes where a cache write comes from.
type source int

const (
	// sourceLocal is a template or sampling rate received by this outlet.
	sourceLocal source = iota
	// sourceKafka is a record read from the shared topic.
	sourceKafka
	// sourceRestore is an entry restored from the local state file.
	sourceRestore
)

// producer is the subset of kgo.Client used by the cache.
type producer interface {
	TryProduce(context.Context, *kgo.Record, func(*kgo.Record, error))
	Close()
}

// Cache is a shared cache for templates and sampling rates.
type Cache struct {
	r      *reporter.Reporter
	config Configuration

	mu        sync.RWMutex
	entries   map[Key]Entry
	lastWrite map[Key]time.Time
	origin    string
	producer  producer

	metrics struct {
		entries        reporter.Gauge
		published      reporter.Counter
		publishErrors  reporter.Counter
		publishDropped reporter.Counter
		received       reporter.Counter
		invalidRecords reporter.Counter
		expired        reporter.Counter
	}
}

// New creates a new template cache.
func New(r *reporter.Reporter, config Configuration) (*Cache, error) {
	config = config.withDefaults()
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	c := &Cache{
		r:         r,
		config:    config,
		entries:   make(map[Key]Entry),
		lastWrite: make(map[Key]time.Time),
		origin:    fmt.Sprintf("%s-%d", hostname, time.Now().UnixNano()),
	}

	c.metrics.entries = c.r.Gauge(reporter.GaugeOpts{
		Name: "entries",
		Help: "Number of entries in the template cache.",
	})
	c.metrics.published = c.r.Counter(reporter.CounterOpts{
		Name: "published_total",
		Help: "Number of records published to the template topic.",
	})
	c.metrics.publishErrors = c.r.Counter(reporter.CounterOpts{
		Name: "publish_errors_total",
		Help: "Number of records that could not be published to the template topic.",
	})
	c.metrics.publishDropped = c.r.Counter(reporter.CounterOpts{
		Name: "publish_dropped_total",
		Help: "Number of records dropped because the publish queue was full.",
	})
	c.metrics.received = c.r.Counter(reporter.CounterOpts{
		Name: "received_total",
		Help: "Number of records received from the template topic.",
	})
	c.metrics.invalidRecords = c.r.Counter(reporter.CounterOpts{
		Name: "invalid_records_total",
		Help: "Number of invalid records received from the template topic.",
	})
	c.metrics.expired = c.r.Counter(reporter.CounterOpts{
		Name: "expired_total",
		Help: "Number of expired entries removed from the template cache.",
	})

	return c, nil
}

// GetTemplate returns a cached template.
func (c *Cache) GetTemplate(key Key) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || entry.Kind == KindSampler || c.expired(entry) {
		return nil, false
	}
	return entry.Template, true
}

// SetTemplate stores a template. It returns true when the entry is new or its
// hash changed.
func (c *Cache) SetTemplate(key Key, template any) (bool, error) {
	kind, data, err := MarshalTemplate(template)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	entry := Entry{
		Kind:      kind,
		Hash:      hex.EncodeToString(sum[:]),
		UpdatedAt: time.Now(),
		Template:  template,
	}
	return c.set(key, entry, sourceLocal), nil
}

// DeleteTemplate removes a template.
func (c *Cache) DeleteTemplate(key Key) error {
	c.delete(key, sourceLocal)
	return nil
}

// DeleteTemplates removes the data or options templates of an exporter, as
// requested by an IPFIX withdraw-all. Sampling rates are never removed.
func (c *Cache) DeleteTemplates(exporter string, version uint16, obsDomainID uint32, options bool) error {
	c.mu.RLock()
	keys := make([]Key, 0)
	for key, entry := range c.entries {
		if key.Exporter != exporter || key.Version != version ||
			key.ObsDomainID != obsDomainID || key.IsSampler {
			continue
		}
		isOptions := entry.Kind == KindIPFIXOptions || entry.Kind == KindNFv9Options
		if isOptions != options {
			continue
		}
		keys = append(keys, key)
	}
	c.mu.RUnlock()
	for _, key := range keys {
		c.delete(key, sourceLocal)
	}
	return nil
}

// GetSamplingRate returns a cached sampling rate.
func (c *Cache) GetSamplingRate(key Key) (uint32, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || entry.Kind != KindSampler || c.expired(entry) {
		return 0, false
	}
	return entry.SamplingRate, true
}

// SetSamplingRate stores a sampling rate.
func (c *Cache) SetSamplingRate(key Key, rate uint32) error {
	entry := Entry{
		Kind:         KindSampler,
		Hash:         strconv.FormatUint(uint64(rate), 10),
		UpdatedAt:    time.Now(),
		SamplingRate: rate,
	}
	c.set(key, entry, sourceLocal)
	return nil
}

// Snapshot returns a copy of the non-expired entries.
func (c *Cache) Snapshot() map[Key]Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries := make(map[Key]Entry, len(c.entries))
	for key, entry := range c.entries {
		if c.expired(entry) {
			continue
		}
		entries[key] = entry
	}
	return entries
}

// Restore merges entries from the local state file into the cache.
func (c *Cache) Restore(entries map[Key]Entry) {
	for key, entry := range entries {
		c.set(key, entry, sourceRestore)
	}
}

// set is the single write path for the cache.
func (c *Cache) set(key Key, entry Entry, src source) bool {
	var publish bool

	c.mu.Lock()
	existing, ok := c.entries[key]
	switch src {
	case sourceLocal:
		entry.Origin = c.origin
		entry.UpdatedAt = maxTime(entry.UpdatedAt, existing.UpdatedAt.Add(time.Nanosecond))
		changed := !ok || existing.Kind != entry.Kind || existing.Hash != entry.Hash
		if changed {
			publish = true
			c.lastWrite[key] = entry.UpdatedAt
		} else if time.Since(c.lastWrite[key]) >= c.config.RefreshInterval {
			entry.UpdatedAt = time.Now()
			publish = true
			c.lastWrite[key] = entry.UpdatedAt
		}
		c.entries[key] = entry
		c.metrics.entries.Set(float64(len(c.entries)))
		c.mu.Unlock()
		if publish {
			c.publish(key, entry)
		}
		return changed

	case sourceKafka:
		if entry.Origin == c.origin {
			c.mu.Unlock()
			return false
		}
		if ok && existing.Origin == c.origin && entry.UpdatedAt.Before(existing.UpdatedAt) {
			c.mu.Unlock()
			return false
		}
		c.entries[key] = entry
		c.lastWrite[key] = time.Now()
		c.metrics.entries.Set(float64(len(c.entries)))
		c.mu.Unlock()
		return true

	case sourceRestore:
		if ok && !entry.UpdatedAt.After(existing.UpdatedAt) {
			c.mu.Unlock()
			return false
		}
		c.entries[key] = entry
		c.lastWrite[key] = time.Now()
		c.metrics.entries.Set(float64(len(c.entries)))
		c.mu.Unlock()
		return true
	}

	c.mu.Unlock()
	return false
}

// delete removes an entry. It returns false and does nothing when the key is
// absent, so no tombstone is published for an absent key.
func (c *Cache) delete(key Key, src source) bool {
	c.mu.Lock()
	if _, ok := c.entries[key]; !ok {
		c.mu.Unlock()
		return false
	}
	delete(c.entries, key)
	delete(c.lastWrite, key)
	c.metrics.entries.Set(float64(len(c.entries)))
	c.mu.Unlock()
	if src == sourceLocal {
		c.publishTombstone(key)
	}
	return true
}

// purge removes expired entries.
func (c *Cache) purge() {
	if c.config.TTL <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for key, entry := range c.entries {
		if now.Sub(entry.UpdatedAt) > c.config.TTL {
			delete(c.entries, key)
			delete(c.lastWrite, key)
			c.metrics.expired.Inc()
		}
	}
	c.metrics.entries.Set(float64(len(c.entries)))
}

// expired tells if an entry is past its TTL.
func (c *Cache) expired(entry Entry) bool {
	return c.config.TTL > 0 && time.Since(entry.UpdatedAt) > c.config.TTL
}

// publish serializes an entry and sends it to the shared topic.
func (c *Cache) publish(key Key, entry Entry) {
	if c.producer == nil {
		return
	}
	value, err := json.Marshal(entry)
	if err != nil {
		c.metrics.publishErrors.Inc()
		c.r.Err(err).Str("key", key.String()).Msg("cannot serialize template cache entry")
		return
	}
	record := &kgo.Record{
		Topic:     c.config.Topic,
		Key:       []byte(key.String()),
		Value:     value,
		Timestamp: time.Now(),
	}
	c.produce(record)
}

// publishTombstone sends a tombstone for the provided key.
func (c *Cache) publishTombstone(key Key) {
	if c.producer == nil {
		return
	}
	record := &kgo.Record{
		Topic:     c.config.Topic,
		Key:       []byte(key.String()),
		Value:     nil,
		Timestamp: time.Now(),
		Headers:   []kgo.RecordHeader{{Key: "origin", Value: []byte(c.origin)}},
	}
	c.produce(record)
}

// produce sends a record and counts the outcome.
func (c *Cache) produce(record *kgo.Record) {
	c.producer.TryProduce(context.Background(), record, func(_ *kgo.Record, err error) {
		switch {
		case err == nil:
			c.metrics.published.Inc()
		case errors.Is(err, kgo.ErrMaxBuffered):
			c.metrics.publishDropped.Inc()
		default:
			c.metrics.publishErrors.Inc()
		}
	})
}

// maxTime returns the later of two times.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
