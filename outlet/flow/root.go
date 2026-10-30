// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

// Package flow handles flow decoding from protobuf messages.
package flow

import (
	"time"

	"gopkg.in/tomb.v2"

	"akvorado/common/pb"
	"akvorado/common/reporter"
	"akvorado/outlet/flow/decoder"
)

// Component represents the flow decoder component.
type Component struct {
	r         *reporter.Reporter
	d         *Dependencies
	config    Configuration
	errLogger reporter.Logger
	t         tomb.Tomb

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
	if c.config.StatePersistFile != "" {
		if err := c.RestoreState(c.config.StatePersistFile); err != nil {
			c.r.Warn().Err(err).Msg("cannot load decoders' state, ignoring")
		} else {
			c.r.Info().Msg("previous decoders' state loaded")
		}

		// Save the state periodically, not only on shutdown: an OOM kill or any
		// other SIGKILL never runs Stop(), leaving the next instance without the
		// templates and sampling rates learned so far.
		c.t.Go(func() error {
			ticker := time.NewTicker(c.config.StatePersistInterval)
			defer ticker.Stop()
			for {
				select {
				case <-c.t.Dying():
					return nil
				case <-ticker.C:
					if err := c.SaveState(c.config.StatePersistFile); err != nil {
						c.r.Err(err).Msg("cannot save decoders' state")
					}
				}
			}
		})
	}
	return nil
}

// Stop stops the flow component
func (c *Component) Stop() error {
	if c.config.StatePersistFile != "" {
		c.t.Kill(nil)
		if err := c.t.Wait(); err != nil {
			c.r.Err(err).Msg("cannot stop decoders' state saver")
		}
		if err := c.SaveState(c.config.StatePersistFile); err != nil {
			c.r.Err(err).Msg("cannot save decorders' state")
		}
	}
	return nil
}
