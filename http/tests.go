package http

import (
	"testing"

	"flowexporter/daemon"
	"flowexporter/reporter"
)

// NewMock create a new HTTP component listening on a random free port.
func NewMock(t *testing.T, r *reporter.Reporter) *Component {
	t.Helper()
	config := DefaultConfiguration
	config.Listen = "127.0.0.1:0"
	c, err := New(r, config, Dependencies{Daemon: daemon.NewMock(t)})
	if err != nil {
		t.Fatalf("New() error:\n%+v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error:\n%+v", err)
	}
	return c
}
