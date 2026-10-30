// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package flow

import "time"

// Configuration describes the configuration for the flow component.
type Configuration struct {
	// StatePersistFile defines a file to store decoder state (templates, sampling
	// rates) to survive restarts.
	StatePersistFile string `validate:"isdefault|filepath"`
	// StatePersistInterval defines how often the decoder state is written to
	// StatePersistFile. Saving only on shutdown is not enough: a killed process
	// (OOM, SIGKILL) never gets the chance to save, and the next instance starts
	// with no templates nor sampling rates, dropping flows until exporters
	// refresh them.
	StatePersistInterval time.Duration `validate:"min=1s"`
}

// DefaultConfiguration returns the default configuration for the flow component.
func DefaultConfiguration() Configuration {
	return Configuration{
		StatePersistInterval: 30 * time.Second,
	}
}
