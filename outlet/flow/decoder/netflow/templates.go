// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package netflow

import (
	"strconv"
	"sync"

	"akvorado/outlet/flow/templatecache"

	"github.com/netsampler/goflow2/v3/decoders/netflow"
)

// templateAndOptionCollection maps exporters to the shared template cache.
type templateAndOptionCollection struct {
	nd    *Decoder
	lock  sync.Mutex
	cache *templatecache.Cache

	Collection map[string]*templatesAndOptions
}

// templatesAndOptions gives an exporter access to the shared cache.
type templatesAndOptions struct {
	nd       *Decoder
	cache    *templatecache.Cache
	exporter string
}

// templateKey identifies a template for the missing-template report.
type templateKey struct {
	version     uint16
	obsDomainID uint32
	templateID  uint16
}

var (
	_ netflow.TemplateStore = &templatesAndOptions{}
)

// Get returns the cache accessor for the provided exporter. If it did not
// exist, it will create a new one.
func (c *templateAndOptionCollection) Get(key string) *templatesAndOptions {
	c.lock.Lock()
	defer c.lock.Unlock()
	t, ok := c.Collection[key]
	if ok {
		return t
	}
	t = &templatesAndOptions{
		nd:       c.nd,
		cache:    c.cache,
		exporter: key,
	}
	c.Collection[key] = t
	return t
}

// GetTemplate returns the requested template.
func (t *templatesAndOptions) GetTemplate(_ netflow.FlowContext, version uint16, obsDomainID uint32, templateID uint16) (any, error) {
	if t.cache == nil {
		return nil, netflow.ErrorTemplateNotFound
	}
	template, ok := t.cache.GetTemplate(templatecache.Key{
		Version:     version,
		Exporter:    t.exporter,
		ObsDomainID: obsDomainID,
		TemplateID:  templateID,
	})
	if !ok {
		return nil, netflow.ErrorTemplateNotFound
	}
	return template, nil
}

// AddTemplate stores a template, or deletes it when it is a withdrawal.
func (t *templatesAndOptions) AddTemplate(_ netflow.FlowContext, version uint16, obsDomainID uint32, templateID uint16, template any) (netflow.TemplateStatus, error) {
	var typeStr string
	switch template.(type) {
	case netflow.IPFIXOptionsTemplateRecord:
		typeStr = "options_template"
	case netflow.NFv9OptionsTemplateRecord:
		typeStr = "options_template"
	case netflow.TemplateRecord:
		typeStr = "template"
	}

	t.nd.metrics.templates.WithLabelValues(
		t.exporter,
		strconv.Itoa(int(version)),
		strconv.Itoa(int(obsDomainID)),
		strconv.Itoa(int(templateID)),
		typeStr,
	).Inc()

	if t.cache == nil {
		return netflow.TemplateAdded, nil
	}
	key := templatecache.Key{
		Version:     version,
		Exporter:    t.exporter,
		ObsDomainID: obsDomainID,
		TemplateID:  templateID,
	}

	// IPFIX withdraw-all: template ID 2 withdraws every data template, template
	// ID 3 every options template, for the exporter and observation domain.
	if version == 10 && (templateID == 2 || templateID == 3) && isEmptyTemplate(template) {
		options := templateID == 3
		if err := t.cache.DeleteTemplates(t.exporter, version, obsDomainID, options); err != nil {
			t.nd.errLogger.Err(err).Str("exporter", t.exporter).Msg("cannot withdraw templates")
			t.nd.metrics.errors.WithLabelValues(t.exporter, "cannot withdraw templates").Inc()
		}
		return netflow.TemplateAdded, nil
	}

	if isEmptyTemplate(template) {
		if err := t.cache.DeleteTemplate(key); err != nil {
			t.nd.errLogger.Err(err).Str("exporter", t.exporter).Msg("cannot delete template")
			t.nd.metrics.errors.WithLabelValues(t.exporter, "cannot delete template").Inc()
		}
		return netflow.TemplateAdded, nil
	}

	// A cache failure must not become flow loss: goflow2 discards the whole
	// packet when AddTemplate returns an error.
	if _, err := t.cache.SetTemplate(key, template); err != nil {
		t.nd.errLogger.Err(err).Str("exporter", t.exporter).Msg("cannot store template")
		t.nd.metrics.errors.WithLabelValues(t.exporter, "cannot store template").Inc()
	}
	return netflow.TemplateAdded, nil
}

// GetSamplingRate returns the requested sampling rate.
func (t *templatesAndOptions) GetSamplingRate(version uint16, obsDomainID uint32, samplerID uint64) uint32 {
	if t.cache == nil {
		return 0
	}
	rate, _ := t.cache.GetSamplingRate(templatecache.Key{
		Version:     version,
		Exporter:    t.exporter,
		ObsDomainID: obsDomainID,
		SamplerID:   samplerID,
		IsSampler:   true,
	})
	return rate
}

// SetSamplingRate sets the sampling rate.
func (t *templatesAndOptions) SetSamplingRate(version uint16, obsDomainID uint32, samplerID uint64, samplingRate uint32) {
	if t.cache == nil {
		return
	}
	if err := t.cache.SetSamplingRate(templatecache.Key{
		Version:     version,
		Exporter:    t.exporter,
		ObsDomainID: obsDomainID,
		SamplerID:   samplerID,
		IsSampler:   true,
	}, samplingRate); err != nil {
		t.nd.errLogger.Err(err).Str("exporter", t.exporter).Msg("cannot store sampling rate")
		t.nd.metrics.errors.WithLabelValues(t.exporter, "cannot store sampling rate").Inc()
	}
}

// isEmptyTemplate tells if a template record is a withdrawal.
func isEmptyTemplate(template any) bool {
	switch template := template.(type) {
	case netflow.TemplateRecord:
		return template.FieldCount == 0
	case netflow.IPFIXOptionsTemplateRecord:
		return template.FieldCount == 0 && template.ScopeFieldCount == 0
	case netflow.NFv9OptionsTemplateRecord:
		return template.OptionLength == 0 && template.ScopeLength == 0
	}
	return false
}
