package protocol

import (
	"strings"
	"testing"
)

func TestValidJobID(t *testing.T) {
	valid := []string{
		"job-abc123",
		"job_daokt5rvplj96hme61p0",
		"a",
		"A.B-c_9",
		strings.Repeat("x", maxJobIDLen),
	}
	for _, id := range valid {
		if err := ValidJobID(id); err != nil {
			t.Errorf("ValidJobID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"slash would break the GET route", "a/b"},
		{"leading slash", "/job-1"},
		{"trailing slash", "job-1/"},
		{"whitespace", "job 1"},
		{"tab", "job\t1"},
		{"dot", "."},
		{"dotdot", ".."},
		{"too long", strings.Repeat("x", maxJobIDLen+1)},
		{"percent", "job%201"},
		{"newline", "job\n1"},
		{"unicode", "jöb"},
	}
	for _, tc := range invalid {
		if err := ValidJobID(tc.id); err == nil {
			t.Errorf("ValidJobID(%q) [%s] = nil, want an error", tc.id, tc.name)
		}
	}
}
