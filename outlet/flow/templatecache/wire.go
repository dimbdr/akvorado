// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package templatecache

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/netsampler/goflow2/v3/decoders/netflow"
)

// Template kinds, also used as the wire format.
const (
	// KindData is a NetFlow v9/IPFIX data template.
	KindData = "data"
	// KindIPFIXOptions is an IPFIX options template.
	KindIPFIXOptions = "ipfix-option"
	// KindNFv9Options is a NetFlow v9 options template.
	KindNFv9Options = "nfv9-option"
	// KindSampler is a sampling rate entry.
	KindSampler = "sampler"
)

// Key identifies a template or a sampling rate in the cache.
type Key struct {
	Version     uint16
	Exporter    string
	ObsDomainID uint32
	TemplateID  uint16
	SamplerID   uint64
	IsSampler   bool
}

// String returns the canonical text representation of the key.
func (k Key) String() string {
	if k.IsSampler {
		return fmt.Sprintf("v%d|%s|%d|sampler:%d", k.Version, k.Exporter, k.ObsDomainID, k.SamplerID)
	}
	return fmt.Sprintf("v%d|%s|%d|%d", k.Version, k.Exporter, k.ObsDomainID, k.TemplateID)
}

// MarshalText implements encoding.TextMarshaler for Key.
func (k Key) MarshalText() ([]byte, error) {
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for Key.
func (k *Key) UnmarshalText(text []byte) error {
	parts := strings.Split(string(text), "|")
	if len(parts) != 4 {
		return fmt.Errorf("invalid template key %q", string(text))
	}
	if len(parts[0]) < 2 || parts[0][0] != 'v' {
		return fmt.Errorf("invalid template key version %q", parts[0])
	}
	version, err := strconv.ParseUint(parts[0][1:], 10, 16)
	if err != nil {
		return fmt.Errorf("invalid template key version %q: %w", parts[0], err)
	}
	if parts[1] == "" {
		return fmt.Errorf("invalid template key %q: empty exporter", string(text))
	}
	obsDomainID, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return fmt.Errorf("invalid template key observation domain %q: %w", parts[2], err)
	}
	key := Key{
		Version:     uint16(version),
		Exporter:    parts[1],
		ObsDomainID: uint32(obsDomainID),
	}
	if sampler, ok := strings.CutPrefix(parts[3], "sampler:"); ok {
		samplerID, err := strconv.ParseUint(sampler, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid template key sampler %q: %w", parts[3], err)
		}
		key.SamplerID = samplerID
		key.IsSampler = true
	} else {
		templateID, err := strconv.ParseUint(parts[3], 10, 16)
		if err != nil {
			return fmt.Errorf("invalid template key template ID %q: %w", parts[3], err)
		}
		key.TemplateID = uint16(templateID)
	}
	*k = key
	return nil
}

// Entry is a cached template or sampling rate.
type Entry struct {
	Kind         string
	Hash         string
	Origin       string
	UpdatedAt    time.Time
	Template     any
	SamplingRate uint32
}

// wireEntry is the JSON representation of an Entry in the Kafka topic.
type wireEntry struct {
	Kind         string          `json:"kind,omitempty"`
	Hash         string          `json:"hash,omitempty"`
	Origin       string          `json:"origin,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at,omitempty"`
	Template     json.RawMessage `json:"template,omitempty"`
	SamplingRate uint32          `json:"sampling_rate,omitempty"`
}

// MarshalJSON implements json.Marshaler for Entry.
func (e Entry) MarshalJSON() ([]byte, error) {
	w := wireEntry{
		Kind:         e.Kind,
		Hash:         e.Hash,
		Origin:       e.Origin,
		UpdatedAt:    e.UpdatedAt,
		SamplingRate: e.SamplingRate,
	}
	if e.Kind != KindSampler {
		_, data, err := MarshalTemplate(e.Template)
		if err != nil {
			return nil, err
		}
		w.Template = data
	}
	return json.Marshal(w)
}

// UnmarshalJSON implements json.Unmarshaler for Entry.
func (e *Entry) UnmarshalJSON(data []byte) error {
	var w wireEntry
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.Kind = w.Kind
	e.Hash = w.Hash
	e.Origin = w.Origin
	e.UpdatedAt = w.UpdatedAt
	e.SamplingRate = w.SamplingRate
	if w.Kind == KindSampler {
		e.Template = nil
		return nil
	}
	if w.Template == nil {
		e.Template = nil
		return nil
	}
	template, err := UnmarshalTemplate(w.Kind, w.Template)
	if err != nil {
		return err
	}
	e.Template = template
	return nil
}

// MarshalTemplate serializes a template and returns its kind.
func MarshalTemplate(t any) (kind string, data json.RawMessage, err error) {
	switch t.(type) {
	case netflow.TemplateRecord:
		kind = KindData
	case netflow.IPFIXOptionsTemplateRecord:
		kind = KindIPFIXOptions
	case netflow.NFv9OptionsTemplateRecord:
		kind = KindNFv9Options
	default:
		return "", nil, fmt.Errorf("unknown template type %T", t)
	}
	data, err = json.Marshal(t)
	return kind, data, err
}

// UnmarshalTemplate deserializes a template of the provided kind.
func UnmarshalTemplate(kind string, data json.RawMessage) (any, error) {
	switch kind {
	case KindData:
		var template netflow.TemplateRecord
		if err := json.Unmarshal(data, &template); err != nil {
			return nil, err
		}
		return template, nil
	case KindIPFIXOptions:
		var template netflow.IPFIXOptionsTemplateRecord
		if err := json.Unmarshal(data, &template); err != nil {
			return nil, err
		}
		return template, nil
	case KindNFv9Options:
		var template netflow.NFv9OptionsTemplateRecord
		if err := json.Unmarshal(data, &template); err != nil {
			return nil, err
		}
		return template, nil
	default:
		return nil, fmt.Errorf("unknown template kind %q", kind)
	}
}
