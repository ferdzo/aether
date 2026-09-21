package internal

import (
	"testing"

	"aether/shared/protocol"
)

// TestJobExitNonceOnlyWhenDelivered pins the invariant that broke the first
// real end-to-end job run: the worker minted an exit nonce and told the scanner
// to expect it, but offline mode never delivers MMDS, so the guest emitted the
// bare AETHER_EXIT:<code> form and the job was recorded as a crash despite
// exiting 42.
func TestJobExitNonceOnlyWhenDelivered(t *testing.T) {
	// Online: MMDS carries the nonce to the guest, so one must be minted.
	if got := jobExitNonce(protocol.Job{}, false); got == "" {
		t.Fatal("an online job should mint a nonce")
	}

	// Offline: no MMDS, so the guest stamps the bare sentinel; minting a nonce
	// would make it unmatchable.
	if got := jobExitNonce(protocol.Job{}, true); got != "" {
		t.Fatalf("an offline job must not mint a nonce, got %q", got)
	}

	// An explicit nonce is honoured either way.
	if got := jobExitNonce(protocol.Job{ExitNonce: "n1"}, true); got != "n1" {
		t.Fatalf("explicit nonce not honoured offline: %q", got)
	}
	if got := jobExitNonce(protocol.Job{ExitNonce: "n2"}, false); got != "n2" {
		t.Fatalf("explicit nonce not honoured online: %q", got)
	}
}
