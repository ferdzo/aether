package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveBootFailClosed(t *testing.T) {
	fetchErr := errors.New("connection refused")

	tests := []struct {
		name       string
		bootToken  string
		argv       []string
		metadata   *MMDSData
		fetchErr   error
		wantErr    bool
		wantArgs   []string
		wantEnv    map[string]string
		wantErrSub string
	}{
		{
			name:       "token plus fetch error fails closed",
			bootToken:  "tok-123",
			argv:       nil,
			metadata:   nil,
			fetchErr:   fetchErr,
			wantErr:    true,
			wantErrSub: "could not be loaded",
		},
		{
			name:       "token but metadata nil fails closed",
			bootToken:  "tok-123",
			argv:       nil,
			metadata:   nil,
			fetchErr:   nil,
			wantErr:    true,
			wantErrSub: "none was returned",
		},
		{
			name:       "token mismatch fails closed",
			bootToken:  "tok-123",
			argv:       nil,
			metadata:   &MMDSData{Token: "other", Entrypoint: "handler.js"},
			fetchErr:   nil,
			wantErr:    true,
			wantErrSub: "token mismatch",
		},
		{
			name:      "no token with argv proceeds",
			bootToken: "",
			argv:      []string{"node", "handler.js"},
			metadata:  nil,
			fetchErr:  nil,
			wantErr:   false,
			wantArgs:  []string{"node", "handler.js"},
			wantEnv:   nil,
		},
		{
			name:       "no token without argv fails closed",
			bootToken:  "",
			argv:       nil,
			metadata:   nil,
			fetchErr:   nil,
			wantErr:    true,
			wantErrSub: "nothing to run",
		},
		{
			name:      "token with metadata dispatches by extension",
			bootToken: "tok-123",
			argv:      nil,
			metadata:  &MMDSData{Token: "tok-123", Entrypoint: "app.js", Env: map[string]string{"A": "B"}},
			fetchErr:  nil,
			wantErr:   false,
			wantArgs:  []string{"node", "app.js"},
			wantEnv:   map[string]string{"A": "B"},
		},
		{
			name:      "token with argv overrides entrypoint",
			bootToken: "tok-123",
			argv:      []string{"python3", "custom.py"},
			metadata:  &MMDSData{Token: "tok-123", Entrypoint: "app.js", Env: map[string]string{"A": "B"}},
			fetchErr:  nil,
			wantErr:   false,
			wantArgs:  []string{"python3", "custom.py"},
			wantEnv:   map[string]string{"A": "B"},
		},
		{
			name:      "token with empty entrypoint defaults to handler.js",
			bootToken: "tok-123",
			argv:      nil,
			metadata:  &MMDSData{Token: "tok-123"},
			fetchErr:  nil,
			wantErr:   false,
			wantArgs:  []string{"node", "handler.js"},
			wantEnv:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, env, err := resolveBoot(tc.bootToken, tc.argv, tc.metadata, tc.fetchErr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (args=%v)", args)
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				if args != nil {
					t.Fatalf("expected nil args on error, got %v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tc.wantArgs)
			}
			if !reflect.DeepEqual(env, tc.wantEnv) {
				t.Fatalf("env = %v, want %v", env, tc.wantEnv)
			}
		})
	}
}

func TestGetCommandForEntrypoint(t *testing.T) {
	tests := []struct {
		entrypoint string
		want       []string
	}{
		{"handler.js", []string{"node", "handler.js"}},
		{"app.py", []string{"python3", "app.py"}},
		{"run.sh", []string{"sh", "run.sh"}},
		{"server", []string{"./server"}},
		{"binary", []string{"./binary"}},
	}

	for _, tc := range tests {
		t.Run(tc.entrypoint, func(t *testing.T) {
			got := getCommandForEntrypoint(tc.entrypoint)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("getCommandForEntrypoint(%q) = %v, want %v", tc.entrypoint, got, tc.want)
			}
		})
	}
}

func TestRenderResolvConf(t *testing.T) {
	tests := []struct {
		name string
		dns  []string
		want string
	}{
		{"nil yields nil", nil, ""},
		{"empty yields nil", []string{}, ""},
		{"blank entries yield nil", []string{"", "   "}, ""},
		{"single server", []string{"10.0.0.1"}, "nameserver 10.0.0.1\n"},
		{"multiple servers", []string{"10.0.0.1", "8.8.8.8"}, "nameserver 10.0.0.1\nnameserver 8.8.8.8\n"},
		{"trims whitespace", []string{" 10.0.0.1 "}, "nameserver 10.0.0.1\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderResolvConf(tc.dns)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("renderResolvConf(%v) = %q, want nil", tc.dns, got)
				}
				return
			}
			if string(got) != tc.want {
				t.Fatalf("renderResolvConf(%v) = %q, want %q", tc.dns, got, tc.want)
			}
		})
	}
}

func TestWriteResolvConfEmptyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "resolv.conf")

	if err := writeResolvConf(path, nil); err != nil {
		t.Fatalf("writeResolvConf with nil dns: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to be created, stat err = %v", path, err)
	}

	if err := writeResolvConf(path, []string{}); err != nil {
		t.Fatalf("writeResolvConf with empty dns: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to be created, stat err = %v", path, err)
	}
}

func TestWriteResolvConfWritesAndCreatesEtc(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "resolv.conf")

	if err := writeResolvConf(path, []string{"10.0.0.1", "8.8.8.8"}); err != nil {
		t.Fatalf("writeResolvConf: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := "nameserver 10.0.0.1\nnameserver 8.8.8.8\n"
	if string(got) != want {
		t.Fatalf("resolv.conf = %q, want %q", got, want)
	}
}
