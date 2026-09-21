package main

import (
	"reflect"
	"testing"
)

func TestParseGuestDNS(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"unset falls back to public resolvers", "", []string{"1.1.1.1", "8.8.8.8"}},
		{"whitespace only falls back", "   ", []string{"1.1.1.1", "8.8.8.8"}},
		{"comma separated", "9.9.9.9,1.0.0.1", []string{"9.9.9.9", "1.0.0.1"}},
		{"trims whitespace and empties", " 9.9.9.9 , , 149.112.112.112 ", []string{"9.9.9.9", "149.112.112.112"}},
		{"no usable entries falls back", ",,", []string{"1.1.1.1", "8.8.8.8"}},
		{"single entry", "8.8.4.4", []string{"8.8.4.4"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGuestDNS(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseGuestDNS(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestDefaultGuestDNSIsNotHostLoopback(t *testing.T) {
	// The host resolver (systemd-resolved) listens on 127.0.0.53, which is not
	// reachable from a guest; the defaults must never be loopback.
	for _, dns := range defaultGuestDNS {
		if dns == "127.0.0.1" || dns == "127.0.0.53" {
			t.Fatalf("default guest DNS must not be loopback: %v", defaultGuestDNS)
		}
	}
}
