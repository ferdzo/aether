package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type MMDSData struct {
	Token      string            `json:"token"`
	Env        map[string]string `json:"env"`
	Entrypoint string            `json:"entrypoint"`
	Port       int               `json:"port"`
	DNS        []string          `json:"dns"`

	// Mode selects how aether-env runs the payload:
	//   ""/"http" -> legacy behaviour: LookPath + syscall.Exec (function mode)
	//   "process" -> guest supervisor: fork the command, wait, sentinel, reboot
	Mode           string   `json:"mode"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeout_s"`
	ExitNonce      string   `json:"exit_nonce"`
}

// Execution modes.
const (
	modeHTTP    = "http"
	modeProcess = "process"
)

// processFlag is the explicit, MMDS-free process-mode entry point:
//
//	aether-env --process <cmd> [args...]
//
// It lets a job rootfs run the supervisor directly, with no boot token, no
// MMDS payload and no network interface. See scripts/build-job-rootfs.sh.
const processFlag = "--process"

// processTimeoutExitCode is the conventional exit code reported when the
// guest-side timeout fires and the supervisor kills the process group.
// (Mirrors coreutils `timeout`, which exits 124 on timeout.)
const processTimeoutExitCode = 124

// Bounded bootstrap retry policy. The worker waits up to 30s for guest
// readiness, so we keep the worst case well under that:
// 3 attempts * 5s HTTP timeout + 0.5s + 1s backoff = 16.5s.
const (
	metadataFetchAttempts = 3
	metadataFetchBackoff  = 500 * time.Millisecond
)

func main() {
	// Explicit, MMDS-free exec-service mode: a long-lived guest control
	// service. It is handled first, before any boot-token/MMDS/HTTP logic, so
	// it works with no boot token and no NIC. Unlike --process it never exits
	// or reboots the guest when a command finishes; see exec_service.go.
	if isExecServiceFlag(os.Args[1:]) {
		fmt.Fprintln(os.Stderr, "aether-env: starting exec service")
		if err := runExecService(); err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: exec service failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Explicit, MMDS-free process mode. This is handled before any boot-token,
	// MMDS or HTTP logic so it works with no boot token and no NIC. The
	// supervisor runs with no timeout and no nonce, so the sentinel is
	// "AETHER_EXIT:<code>".
	if cmd, ok, err := parseProcessModeFlag(os.Args[1:]); ok {
		if err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: refusing to start: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "aether-env: supervisor running %v (timeout=0s, nonce=\"\")\n", cmd)
		if err := runSupervisor(cmd, 0, ""); err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: supervisor failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	bootToken := parseBootToken()

	var argv []string
	if len(os.Args) > 1 {
		argv = os.Args[1:]
	}

	// Only fetch bootstrap metadata when the kernel command line told us a
	// token is expected. This is the "bootstrap was expected" signal.
	var (
		metadata *MMDSData
		fetchErr error
	)
	if bootToken != "" {
		metadata, fetchErr = fetchMetadataWithRetry(bootToken, metadataFetchAttempts, metadataFetchBackoff)
	}

	args, env, err := resolveBoot(bootToken, argv, metadata, fetchErr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: refusing to start: %v\n", err)
		os.Exit(1)
	}

	// Set env vars.
	for key, value := range env {
		os.Setenv(key, value)
	}

	// DNS is optional. An empty list preserves the previous behavior of
	// leaving /etc/resolv.conf untouched.
	if metadata != nil && len(metadata.DNS) > 0 {
		if err := writeResolvConf("/etc/resolv.conf", metadata.DNS); err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: warning: failed to write /etc/resolv.conf: %v\n", err)
		}
	}

	if resolveMode(metadata) == modeProcess {
		cmd, err := resolveProcessCommand(metadata, argv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: refusing to start: %v\n", err)
			os.Exit(1)
		}

		var timeout time.Duration
		var nonce string
		if metadata != nil {
			if metadata.TimeoutSeconds > 0 {
				timeout = time.Duration(metadata.TimeoutSeconds) * time.Second
			}
			nonce = metadata.ExitNonce
		}

		fmt.Fprintf(os.Stderr, "aether-env: supervisor running %v (timeout=%s, nonce=%q)\n", cmd, timeout, nonce)
		if err := runSupervisor(cmd, timeout, nonce); err != nil {
			fmt.Fprintf(os.Stderr, "aether-env: supervisor failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	binary, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: command not found: %s\n", args[0])
		os.Exit(127)
	}

	fmt.Fprintf(os.Stderr, "aether-env: running %v\n", args)
	if err := syscall.Exec(binary, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: exec failed: %v\n", err)
		os.Exit(1)
	}
}

// parseProcessModeFlag recognises the explicit, MMDS-free process entry point.
//
// argv is the process arguments after the program name (os.Args[1:]).
//
//   - ok=false: the flag was not present. The caller continues with the
//     existing boot-token/MMDS/HTTP logic, so a bare command such as
//     `aether-env node handler.js` is completely unchanged.
//   - ok=true and err!=nil: the flag was present but no command followed it.
//   - ok=true and err==nil: the caller runs runSupervisor on cmd with no
//     timeout and no nonce (sentinel "AETHER_EXIT:<code>").
//
// Only the first argument is inspected, so the flag cannot be smuggled in as a
// program argument.
func parseProcessModeFlag(argv []string) (cmd []string, ok bool, err error) {
	if len(argv) == 0 || argv[0] != processFlag {
		return nil, false, nil
	}
	if len(argv) < 2 {
		return nil, true, fmt.Errorf("%s requires a command (for example: aether-env %s sh -c 'echo hi')", processFlag, processFlag)
	}
	return argv[1:], true, nil
}

// resolveMode normalises the MMDS mode field into one of the two execution
// modes. An empty mode (the historical function payload has no mode field) and
// an explicit "http" both select the existing syscall.Exec path; any other
// value also falls back to that path so the function flow keeps working
// unchanged. "process" selects the guest supervisor.
func resolveMode(metadata *MMDSData) string {
	if metadata != nil && metadata.Mode == modeProcess {
		return modeProcess
	}
	return modeHTTP
}

// resolveProcessCommand is the pure command-selection function for process
// mode. Preference order:
//  1. metadata.Command when non-empty (the job specifies its argv explicitly)
//  2. argv when present (an explicit /init argument)
//  3. otherwise fail: process mode never defaults to handler.js, because that
//     would silently run the wrong thing for a job.
func resolveProcessCommand(metadata *MMDSData, argv []string) ([]string, error) {
	if metadata != nil && len(metadata.Command) > 0 {
		return metadata.Command, nil
	}
	if len(argv) > 0 {
		return argv, nil
	}
	return nil, errors.New("process mode: no command provided (MMDS command is empty and no argv was given)")
}

// formatExitSentinel renders the exit-status sentinel a host-side scanner
// matches. With a nonce: "AETHER_EXIT:<nonce>:<code>"; without: "AETHER_EXIT:<code>".
func formatExitSentinel(nonce string, code int) string {
	if nonce != "" {
		return fmt.Sprintf("AETHER_EXIT:%s:%d", nonce, code)
	}
	return fmt.Sprintf("AETHER_EXIT:%d", code)
}

// exitCodeFromWaitStatus maps a wait status to a conventional exit code:
// 128+signal for a signalled process, otherwise the exit status itself.
func exitCodeFromWaitStatus(status syscall.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

// exitCodeFromState maps a finished process state to the exit code the
// supervisor reports. It uses the real exit code and, when the process was
// signalled, the conventional 128+signal value.
func exitCodeFromState(state *os.ProcessState) int {
	if state == nil {
		return 1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok {
		return exitCodeFromWaitStatus(status)
	}
	return state.ExitCode()
}

// runSupervisor starts cmd as a child of this process (PID 1) with stdout and
// stderr inherited from the guest serial console, waits for it (enforcing the
// guest-side timeout), then flushes, prints the exit sentinel and resets the
// VM.
//
// Conventions:
//   - timeout > 0: on expiry the whole process group is SIGKILLed and the
//     reported exit code is processTimeoutExitCode (124).
//   - the child runs in its own process group (Setpgid) so that signalled
//     cleanup reaches grandchildren too.
//   - after the child exits, the process group is killed again to reap any
//     straggler children, then syscall.Sync flushes filesystem writes before
//     the sentinel is printed.
//   - the sentinel is the final stdout line; reboot(RB_AUTOBOOT) terminates
//     the VM. poweroff does not stop Firecracker on x86, so a reboot is
//     required (the kernel command line carries reboot=k).
func runSupervisor(cmd []string, timeout time.Duration, nonce string) error {
	child := exec.Command(cmd[0], cmd[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = os.Environ()
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	exitCode := 1
	started := false
	if err := child.Start(); err != nil {
		// A command that cannot be started (e.g. not found) is reported as
		// 127, matching the shell convention, so the VM still terminates with
		// a sentinel instead of waiting for the host deadline.
		fmt.Fprintf(os.Stderr, "aether-env: failed to start %v: %v\n", cmd, err)
		exitCode = 127
	} else {
		started = true
		exitCode = superviseChild(child, timeout)
	}

	if started {
		// Best effort: reap any straggler children still in the group. The
		// child has already exited, so ESRCH is expected and ignored.
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
	}

	// Flush filesystem writes before announcing completion.
	syscall.Sync()

	// Final stdout line before the VM resets, so a host scanner can match
	// the nonce. stdout is unbuffered, so no explicit drain is needed.
	fmt.Fprintf(os.Stdout, "%s\n", formatExitSentinel(nonce, exitCode))

	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: reboot failed: %v\n", err)
		return err
	}
	return nil
}

// superviseChild waits for the child to exit, enforcing the guest-side
// timeout. It returns the exit code, or processTimeoutExitCode when the
// timeout fired and the process group was killed.
func superviseChild(child *exec.Cmd, timeout time.Duration) int {
	done := make(chan struct{})
	go func() {
		_ = child.Wait()
		close(done)
	}()

	if timeout <= 0 {
		<-done
		return exitCodeFromState(child.ProcessState)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return exitCodeFromState(child.ProcessState)
	case <-timer.C:
		// Kill the whole process group so children spawned by the workload
		// cannot outlive the timeout, then reap the direct child.
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		<-done
		return processTimeoutExitCode
	}
}

// resolveBoot is the pure fail-closed decision function. It selects the
// command to exec and the environment to apply, given:
//   - bootToken: the token parsed from the kernel command line ("" if absent)
//   - argv: the explicit command from os.Args[1:] (nil if absent)
//   - metadata: the MMDS payload (nil if it was not fetched)
//   - fetchErr: the error from fetching/validating the MMDS payload
//
// Policy:
//   - token present and metadata could not be loaded/verified -> error
//     (never fall back to handler.js)
//   - token absent and argv present -> legacy explicit-command path
//   - token absent and no argv -> error (nothing legitimate to run)
func resolveBoot(bootToken string, argv []string, metadata *MMDSData, fetchErr error) ([]string, map[string]string, error) {
	hasArgv := len(argv) > 0

	if bootToken != "" {
		if fetchErr != nil {
			return nil, nil, fmt.Errorf("bootstrap metadata was expected (boot token present) but could not be loaded: %w", fetchErr)
		}
		if metadata == nil {
			return nil, nil, errors.New("bootstrap metadata was expected (boot token present) but none was returned")
		}
		if metadata.Token != bootToken {
			return nil, nil, fmt.Errorf("bootstrap token mismatch (expected %q, got %q)", bootToken, metadata.Token)
		}
		if hasArgv {
			return argv, metadata.Env, nil
		}
		entrypoint := metadata.Entrypoint
		if entrypoint == "" {
			entrypoint = "handler.js"
		}
		return getCommandForEntrypoint(entrypoint), metadata.Env, nil
	}

	if hasArgv {
		// Legacy explicit-command path, e.g. aether-env node handler.js.
		return argv, nil, nil
	}

	return nil, nil, errors.New("no boot token and no command provided; nothing to run")
}

func getCommandForEntrypoint(entrypoint string) []string {
	switch {
	case strings.HasSuffix(entrypoint, ".js"):
		return []string{"node", entrypoint}
	case strings.HasSuffix(entrypoint, ".py"):
		return []string{"python3", entrypoint}
	case strings.HasSuffix(entrypoint, ".sh"):
		return []string{"sh", entrypoint}
	default:
		// Assume it's an executable
		return []string{"./" + entrypoint}
	}
}

func parseBootToken() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}

	for _, arg := range strings.Fields(string(data)) {
		if strings.HasPrefix(arg, "aether_token=") {
			return strings.TrimPrefix(arg, "aether_token=")
		}
	}
	return ""
}

// fetchMetadataWithRetry calls fetchMetadata up to attempts times, sleeping
// with an exponential backoff between tries. It returns the last error if all
// attempts fail.
func fetchMetadataWithRetry(bootToken string, attempts int, baseBackoff time.Duration) (*MMDSData, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(baseBackoff * time.Duration(1<<(i-1)))
		}
		metadata, err := fetchMetadata(bootToken)
		if err == nil {
			return metadata, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// renderResolvConf renders the contents of a resolv.conf from a list of
// nameserver addresses. It returns nil when there is nothing to write, which
// signals to the caller that /etc/resolv.conf must be left untouched.
func renderResolvConf(dns []string) []byte {
	var b strings.Builder
	for _, ip := range dns {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		fmt.Fprintf(&b, "nameserver %s\n", ip)
	}
	if b.Len() == 0 {
		return nil
	}
	return []byte(b.String())
}

// writeResolvConf writes /etc/resolv.conf from the given DNS list. An empty or
// whitespace-only list is a no-op.
func writeResolvConf(path string, dns []string) error {
	content := renderResolvConf(dns)
	if content == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}

func fetchMetadata(bootToken string) (*MMDSData, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequest("GET", "http://169.254.169.254/", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch MMDS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("MMDS returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read MMDS response: %w", err)
	}

	var metadata MMDSData
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, fmt.Errorf("invalid JSON from MMDS (got: %.100s): %w", string(body), err)
	}

	if metadata.Token != bootToken {
		return nil, fmt.Errorf("token mismatch (expected %s, got %s)", bootToken, metadata.Token)
	}

	return &metadata, nil
}
