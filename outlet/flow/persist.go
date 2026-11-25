// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package flow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"akvorado/outlet/flow/templatecache"

	"github.com/google/renameio/v2"
)

// ErrStateVersion is triggered when loading a collection from an incompatible version
var ErrStateVersion = errors.New("collection version mismatch")

// currentStateVersionNumber should be increased each time we change the way we
// encode the collection.
const currentStateVersionNumber = 2

// SaveState saves the template cache to a file. This is not goroutine-safe.
func (c *Component) SaveState(target string) error {
	state := struct {
		Version   int
		Templates map[templatecache.Key]templatecache.Entry
	}{
		Version:   currentStateVersionNumber,
		Templates: c.cache.Snapshot(),
	}
	data, err := json.Marshal(&state)
	if err != nil {
		return fmt.Errorf("unable to encode decoders' state: %w", err)
	}
	if err := renameio.WriteFile(target, data, 0o666, renameio.WithTempDir(filepath.Dir(target))); err != nil {
		return fmt.Errorf("unable to write state file %q: %w", target, err)
	}
	return nil
}

// RestoreState restores the template cache from a file. This is not goroutine-safe.
func (c *Component) RestoreState(source string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("unable to read state file %q: %w", source, err)
	}

	// Check the version.
	var stateVersion struct {
		Version int
	}
	if err := json.Unmarshal(data, &stateVersion); err != nil {
		return err
	}
	if stateVersion.Version != currentStateVersionNumber {
		return ErrStateVersion
	}

	// Decode the cache snapshot.
	var state struct {
		Templates map[templatecache.Key]templatecache.Entry
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("unable to decode decoders' state: %w", err)
	}
	c.cache.Restore(state.Templates)
	return nil
}
