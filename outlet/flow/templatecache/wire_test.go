// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package templatecache

import (
	"testing"

	"akvorado/common/helpers"

	"github.com/netsampler/goflow2/v3/decoders/netflow"
)

func TestTemplateWireRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"data", netflow.TemplateRecord{TemplateId: 256, FieldCount: 2, Fields: []netflow.Field{{Type: 8, Length: 4}, {Type: 12, Length: 4}}}},
		{"ipfix-option", netflow.IPFIXOptionsTemplateRecord{TemplateId: 257, FieldCount: 2, ScopeFieldCount: 1, Scopes: []netflow.Field{{Type: 1, Length: 4}}, Options: []netflow.Field{{Type: 2, Length: 4}}}},
		{"nfv9-option", netflow.NFv9OptionsTemplateRecord{TemplateId: 258, ScopeLength: 4, OptionLength: 4, Scopes: []netflow.Field{{Type: 1, Length: 4}}, Options: []netflow.Field{{Type: 2, Length: 4}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, data, err := MarshalTemplate(tc.in)
			if err != nil {
				t.Fatalf("MarshalTemplate() error: %v", err)
			}
			got, err := UnmarshalTemplate(kind, data)
			if err != nil {
				t.Fatalf("UnmarshalTemplate() error: %v", err)
			}
			if diff := helpers.Diff(got, tc.in); diff != "" {
				t.Fatalf("round-trip (-got, +want):\n%s", diff)
			}
		})
	}
}

func TestKeyFormat(t *testing.T) {
	if got, want := (Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}).String(), "v9|192.0.2.10|300|256"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := (Key{Version: 10, Exporter: "::ffff:192.0.2.10", ObsDomainID: 1, SamplerID: 10, IsSampler: true}).String(), "v10|::ffff:192.0.2.10|1|sampler:10"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	var key Key
	if err := key.UnmarshalText([]byte("v9|192.0.2.10|300|256")); err != nil {
		t.Fatalf("UnmarshalText() error: %v", err)
	}
	if diff := helpers.Diff(key, Key{Version: 9, Exporter: "192.0.2.10", ObsDomainID: 300, TemplateID: 256}); diff != "" {
		t.Fatalf("UnmarshalText() (-got, +want):\n%s", diff)
	}
	var sampler Key
	if err := sampler.UnmarshalText([]byte("v10|192.0.2.10|1|sampler:10")); err != nil {
		t.Fatalf("UnmarshalText() error: %v", err)
	}
	if diff := helpers.Diff(sampler, Key{Version: 10, Exporter: "192.0.2.10", ObsDomainID: 1, SamplerID: 10, IsSampler: true}); diff != "" {
		t.Fatalf("UnmarshalText() sampler (-got, +want):\n%s", diff)
	}
}
