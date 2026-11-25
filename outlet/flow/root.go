// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

// Package flow handles flow decoding from protobuf messages.
package flow

import (
	"fmt"
	"time"

	"akvorado/common/pb"
	"akvorado/common/reporter"
	"akvorado/outlet/flow/decoder"
	"akvorado/outlet/flow/templatecache"
)

// Component represents the flow decoder component.
type Component struct {
	r         *reporter.Reporter
	d         *Dependencies
	config    Configuration
	errLogger reporter.Logger
	cache     *templatecache.Cache

	metrics struct {
		decoderStats  *reporter.CounterVec
		decoderErrors *reporter.CounterVec
	}

	// Available decoders
	decoders map[pb.RawFlow_Decoder]decoder.Decoder
}

// Dependencies are the dependencies of the flow component.
type Dependencies = decoder.Dependencies

// New creates a new flow component.
func New(r *reporter.Reporter, config Configuration, dependencies Dependencies) (*Component, error) {
	c := Component{
		r:         r,
		d:         &dependencies,
		config:    config,
		errLogger: r.Sample(reporter.BurstSampler(30*time.Second, 3)),
		decoders:  make(map[pb.RawFlow_Decoder]decoder.Decoder),
	}

	// Create the shared template cache and hand it to every decoder.
	cacheConfig := templatecache.DefaultConfiguration()
	if config.TemplateCache != nil {
		cacheConfig = *config.TemplateCache
	}
	cache, err := templatecache.New(r, cacheConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create template cache: %w", err)
	}
	c.cache = cache
	dependencies.TemplateCache = cache

	// Initialize available decoders
	for decoderType, decoderFunc := range availableDecoders {
		c.decoders[decoderType] = decoderFunc(r, dependencies)
	}

	// Metrics
	c.metrics.decoderStats = c.r.CounterVec(
		reporter.CounterOpts{
			Name: "decoder_flows_total",
			Help: "Decoder processed count.",
		},
		[]string{"name"},
	)
	c.metrics.decoderErrors = c.r.CounterVec(
		reporter.CounterOpts{
			Name: "decoder_errors_total",
			Help: "Decoder processed error count.",
		},
		[]string{"name"},
	)

	return &c, nil
}

// Start starts the flow component.
func (c *Component) Start() error {
	// The preload must finish before the flow workers start decoding.
	if err := c.cache.Start(); err != nil {
		c.r.Warn().Err(err).Msg("cannot start template cache, continuing without it")
	}
	if c.config.StatePersistFile != "" {
		if err := c.RestoreState(c.config.StatePersistFile); err != nil {
			c.r.Warn().Err(err).Msg("cannot load decoders' state, ignoring")
		} else {
			c.r.Info().Msg("previous decoders' state loaded")
		}
	}
	return nil
}

// Stop stops the flow component
func (c *Component) Stop() error {
	if c.config.StatePersistFile != "" {
		if err := c.SaveState(c.config.StatePersistFile); err != nil {
			c.r.Err(err).Msg("cannot save decorders' state")
		}
	}
	if err := c.cache.Stop(); err != nil {
		c.r.Warn().Err(err).Msg("cannot stop template cache")
	}
	return nil
}
