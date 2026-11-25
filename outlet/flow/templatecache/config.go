// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

// Package templatecache provides a shared cache for NetFlow/IPFIX templates and
// sampling rates. The cache is backed by a compacted Kafka topic so that an
// outlet can decode flows whose templates were received by another outlet.
package templatecache

import (
	"time"

	"akvorado/common/kafka"
)

// Configuration describes the configuration for the template cache.
type Configuration struct {
	// Enabled tells if the shared template cache is enabled.
	Enabled             bool
	kafka.Configuration `mapstructure:",squash" yaml:",inline"`
	// TTL is the maximum age of an entry before it is considered expired. Zero
	// disables expiry; compaction deduplicates entries in Kafka.
	TTL time.Duration
	// RefreshInterval is how often an unchanged template is republished to
	// keep it alive in the compacted topic.
	RefreshInterval time.Duration `validate:"omitempty,min=1"`
	// PreloadTimeout bounds how long the cache waits for the Kafka preload.
	PreloadTimeout time.Duration `validate:"omitempty,min=1"`
	// QueueSize is the maximum number of records buffered before dropping.
	QueueSize int `validate:"omitempty,min=1"`
}

// DefaultConfiguration returns the default configuration for the template cache.
func DefaultConfiguration() Configuration {
	return Configuration{
		Enabled: false,
		Configuration: kafka.Configuration{
			Topic:   "templates",
			Brokers: []string{"127.0.0.1:9092"},
		},
		TTL:             0,
		RefreshInterval: 15 * time.Minute,
		PreloadTimeout:  30 * time.Second,
		QueueSize:       4096,
	}
}

// withDefaults fills the zero fields of a configuration with their defaults.
// This covers a hand-written flow.template-cache block or a test literal that
// does not set every field.
func (c Configuration) withDefaults() Configuration {
	defaults := DefaultConfiguration()
	if c.Topic == "" {
		c.Topic = defaults.Topic
	}
	if len(c.Brokers) == 0 {
		c.Brokers = defaults.Brokers
	}
	if c.QueueSize < 1 {
		c.QueueSize = defaults.QueueSize
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = defaults.RefreshInterval
	}
	if c.PreloadTimeout <= 0 {
		c.PreloadTimeout = defaults.PreloadTimeout
	}
	return c
}
