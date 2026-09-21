package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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

func TestParseProcessModeFlag(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		wantOK     bool
		wantCmd    []string
		wantErr    bool
		wantErrSub string
	}{
		{
			name:    "process flag selects the supervisor",
			argv:    []string{"--process", "sh", "-c", "echo hello; sleep 2; exit 42"},
			wantOK:  true,
			wantCmd: []string{"sh", "-c", "echo hello; sleep 2; exit 42"},
		},
		{
			name:    "process flag with single command",
			argv:    []string{"--process", "true"},
			wantOK:  true,
			wantCmd: []string{"true"},
		},
		{
			name:       "process flag without a command fails",
			argv:       []string{"--process"},
			wantOK:     true,
			wantErr:    true,
			wantErrSub: "requires a command",
		},
		{
			// A bare command must not select the supervisor: the caller keeps
			// going down the existing boot-token/MMDS/HTTP path.
			name:    "bare command does not select the supervisor",
			argv:    []string{"node", "handler.js"},
			wantOK:  false,
			wantCmd: nil,
		},
		{
			name:    "empty argv does not select the supervisor",
			argv:    nil,
			wantOK:  false,
			wantCmd: nil,
		},
		{
			// Only argv[0] is inspected, so the flag after a program name is
			// treated as an argument to that program, not as process mode.
			name:    "flag not in first position does not select the supervisor",
			argv:    []string{"node", "--process"},
			wantOK:  false,
			wantCmd: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, ok, err := parseProcessModeFlag(tc.argv)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (cmd=%v)", cmd)
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(cmd, tc.wantCmd) {
				t.Fatalf("cmd = %v, want %v", cmd, tc.wantCmd)
			}
		})
	}
}

// A bare argv (no --process) must continue to resolve to the HTTP path, i.e.
// the explicit process entry point must not leak into the legacy flow.
func TestBareCommandKeepsHTTPPath(t *testing.T) {
	cmd, ok, err := parseProcessModeFlag([]string{"node", "handler.js"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("bare command selected process mode (cmd=%v)", cmd)
	}
	if got := resolveMode(nil); got != modeHTTP {
		t.Fatalf("resolveMode(nil) = %q, want %q", got, modeHTTP)
	}
}

func TestResolveMode(t *testing.T) {
	tests := []struct {
		name     string
		metadata *MMDSData
		want     string
	}{
		{"nil metadata selects http", nil, modeHTTP},
		{"empty mode selects http", &MMDSData{}, modeHTTP},
		{"explicit http selects http", &MMDSData{Mode: "http"}, modeHTTP},
		{"process selects supervisor", &MMDSData{Mode: "process"}, modeProcess},
		{"unknown mode falls back to http", &MMDSData{Mode: "bogus"}, modeHTTP},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMode(tc.metadata); got != tc.want {
				t.Fatalf("resolveMode(%+v) = %q, want %q", tc.metadata, got, tc.want)
			}
		})
	}
}

func TestResolveProcessCommand(t *testing.T) {
	tests := []struct {
		name       string
		metadata   *MMDSData
		argv       []string
		want       []string
		wantErr    bool
		wantErrSub string
	}{
		{
			name:     "MMDS command wins over argv",
			metadata: &MMDSData{Command: []string{"echo", "from-mmds"}},
			argv:     []string{"echo", "from-argv"},
			want:     []string{"echo", "from-mmds"},
		},
		{
			name:     "falls back to argv when command empty",
			metadata: &MMDSData{},
			argv:     []string{"sh", "-c", "true"},
			want:     []string{"sh", "-c", "true"},
		},
		{
			name:     "falls back to argv when metadata nil",
			metadata: nil,
			argv:     []string{"python3", "script.py"},
			want:     []string{"python3", "script.py"},
		},
		{
			name:       "errors when both empty",
			metadata:   &MMDSData{},
			argv:       nil,
			wantErr:    true,
			wantErrSub: "no command provided",
		},
		{
			name:       "errors when metadata nil and no argv",
			metadata:   nil,
			argv:       nil,
			wantErr:    true,
			wantErrSub: "no command provided",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProcessCommand(tc.metadata, tc.argv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (cmd=%v)", got)
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				if got != nil {
					t.Fatalf("expected nil command on error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveProcessCommand = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatExitSentinel(t *testing.T) {
	tests := []struct {
		name  string
		nonce string
		code  int
		want  string
	}{
		{"with nonce", "nonce-abc", 0, "AETHER_EXIT:nonce-abc:0"},
		{"with nonce and non-zero code", "nonce-abc", 137, "AETHER_EXIT:nonce-abc:137"},
		{"without nonce", "", 124, "AETHER_EXIT:124"},
		{"without nonce and zero code", "", 0, "AETHER_EXIT:0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatExitSentinel(tc.nonce, tc.code); got != tc.want {
				t.Fatalf("formatExitSentinel(%q, %d) = %q, want %q", tc.nonce, tc.code, got, tc.want)
			}
		})
	}
}

func TestExitCodeFromWaitStatus(t *testing.T) {
	tests := []struct {
		name   string
		status syscall.WaitStatus
		want   int
	}{
		{"normal exit zero", syscall.WaitStatus(0), 0},
		{"normal exit code", syscall.WaitStatus(42 << 8), 42},
		{"signalled SIGKILL", syscall.WaitStatus(syscall.SIGKILL), 128 + int(syscall.SIGKILL)},
		{"signalled SIGTERM", syscall.WaitStatus(syscall.SIGTERM), 128 + int(syscall.SIGTERM)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFromWaitStatus(tc.status); got != tc.want {
				t.Fatalf("exitCodeFromWaitStatus(%d) = %d, want %d", tc.status, got, tc.want)
			}
		})
	}
}

// TestExitCodeFromStateNormalExit exercises the wrapper with a real short-lived
// child purely to obtain an *os.ProcessState carrying a known exit code; it
// neither touches MMDS nor reboots. The signalled mapping is covered purely by
// TestExitCodeFromWaitStatus.
func TestExitCodeFromStateNormalExit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	cmd := exec.Command("sh", "-c", "exit 42")
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("running helper child: %v", err)
		}
	}
	if cmd.ProcessState == nil {
		t.Fatal("expected ProcessState to be populated")
	}
	if got := exitCodeFromState(cmd.ProcessState); got != 42 {
		t.Fatalf("exitCodeFromState = %d, want 42", got)
	}
}

func TestExitCodeFromStateNil(t *testing.T) {
	if got := exitCodeFromState(nil); got != 1 {
		t.Fatalf("exitCodeFromState(nil) = %d, want 1", got)
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
