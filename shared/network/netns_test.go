package network

import (
	"net"
	"testing"
)

func TestSubnetBaseProgression(t *testing.T) {
	_, supernet, _ := net.ParseCIDR("172.31.0.0/24") // 256 addrs = 64 /30s

	want := map[int]string{
		0:  "172.31.0.0",
		1:  "172.31.0.4",
		63: "172.31.0.252",
	}
	for idx, expect := range want {
		got, err := subnetBase(supernet, idx)
		if err != nil {
			t.Fatalf("idx %d: %v", idx, err)
		}
		if got.String() != expect {
			t.Fatalf("idx %d = %s, want %s", idx, got, expect)
		}
	}

	// Larger supernet must carry across octet boundaries correctly:
	// subnet 256 × 4 addresses = 1024 = four /24s past the base.
	_, big, _ := net.ParseCIDR("172.31.0.0/16")
	got, err := subnetBase(big, 256)
	if err != nil {
		t.Fatalf("idx 256 in /16: %v", err)
	}
	if got.String() != "172.31.4.0" {
		t.Fatalf("idx 256 = %s, want 172.31.4.0", got)
	}
}

func TestSubnetBaseExhaustion(t *testing.T) {
	_, supernet, _ := net.ParseCIDR("172.31.0.5/30") // single-subnet pool
	if _, err := subnetBase(supernet, 0); err != nil {
		t.Fatalf("first allocation should succeed: %v", err)
	}
	if _, err := subnetBase(supernet, 1); err == nil {
		t.Fatal("expected exhaustion error past pool size")
	}

	// Misaligned/tiny input rejected by the manager constructor.
	if _, err := NewNetnsManager("10.0.0.1/32"); err == nil {
		t.Fatal("/32 supernet must be rejected")
	}
}

func TestAddrOffsetSemantics(t *testing.T) {
	base := net.IPv4(172, 31, 0, 4).To4()
	host := addrOffset(base, 1)
	guest := addrOffset(base, 2)
	if host != "172.31.0.5" || guest != "172.31.0.6" {
		t.Fatalf("host=%s guest=%s", host, guest)
	}
	if base.String() != "172.31.0.4" {
		t.Fatalf("base mutated: %s", base)
	}
}
