package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestIsExecServiceFlag(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{"exec-service flag", []string{"--exec-service"}, true},
		{"extra args still select the service", []string{"--exec-service", "ignored"}, true},
		{"process flag does not", []string{"--process", "true"}, false},
		{"bare command does not", []string{"node", "handler.js"}, false},
		{"empty argv does not", nil, false},
		{"flag not first does not", []string{"node", "--exec-service"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecServiceFlag(tc.argv); got != tc.want {
				t.Fatalf("isExecServiceFlag(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/workspace"}

	t.Run("no overrides returns base", func(t *testing.T) {
		if got := mergeEnv(base, nil); !reflect.DeepEqual(got, base) {
			t.Fatalf("mergeEnv = %v, want %v", got, base)
		}
	})

	t.Run("override replaces and append adds", func(t *testing.T) {
		got := mergeEnv(base, map[string]string{"HOME": "/root", "FOO": "bar"})
		if len(got) != 3 {
			t.Fatalf("mergeEnv = %v, want 3 entries", got)
		}
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, "HOME=/root") {
			t.Fatalf("override not applied: %v", got)
		}
		if strings.Contains(joined, "HOME=/workspace") {
			t.Fatalf("old value still present: %v", got)
		}
		if !strings.Contains(joined, "FOO=bar") {
			t.Fatalf("new var not appended: %v", got)
		}
		if got[0] != "PATH=/bin" || got[1] != "HOME=/root" {
			t.Fatalf("base order not preserved: %v", got)
		}
	})
}
