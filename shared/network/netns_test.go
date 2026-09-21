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

// TestPlanInstanceAddresses pins the addressing contract: within each instance
// the guest /30 owns the gateway (.1) and guest (.2) addresses, while host<->netns
// connectivity uses a completely separate transport /30 indexed the same way.
func TestPlanInstanceAddresses(t *testing.T) {
	_, supernet, _ := net.ParseCIDR("172.31.0.0/16")
	_, transport, _ := net.ParseCIDR(transportSupernetCIDR)

	cases := []struct {
		idx                             int
		host, guest, xportHost, xportNS string
	}{
		{0, "172.31.0.1", "172.31.0.2", "169.254.0.1", "169.254.0.2"},
		{1, "172.31.0.5", "172.31.0.6", "169.254.0.5", "169.254.0.6"},
		{64, "172.31.1.1", "172.31.1.2", "169.254.1.1", "169.254.1.2"},
	}

	for _, tc := range cases {
		p, err := planInstanceAddresses(supernet, transport, tc.idx)
		if err != nil {
			t.Fatalf("idx %d: %v", tc.idx, err)
		}
		if p.HostIP != tc.host || p.GuestIP != tc.guest {
			t.Fatalf("idx %d guest plan = host %s guest %s, want %s/%s", tc.idx, p.HostIP, p.GuestIP, tc.host, tc.guest)
		}
		if p.TransportHost != tc.xportHost || p.TransportNS != tc.xportNS {
			t.Fatalf("idx %d transport plan = %s/%s, want %s/%s", tc.idx, p.TransportHost, p.TransportNS, tc.xportHost, tc.xportNS)
		}
	}
}

// TestPlanInstanceAddressesPoolsAreIndependent verifies the transport pool never
// consumes the guest /30 addresses (a regression that made .1/.2 unusable).
func TestPlanInstanceAddressesPoolsAreIndependent(t *testing.T) {
	_, supernet, _ := net.ParseCIDR("172.31.0.0/16")
	_, transport, _ := net.ParseCIDR(transportSupernetCIDR)

	p, err := planInstanceAddresses(supernet, transport, 0)
	if err != nil {
		t.Fatal(err)
	}

	guestIP := net.ParseIP(p.GuestIP)
	if supernet.Contains(net.ParseIP(p.TransportHost)) || supernet.Contains(net.ParseIP(p.TransportNS)) {
		t.Fatalf("transport addressing overlaps the guest supernet")
	}
	if !supernet.Contains(guestIP) {
		t.Fatalf("guest address %s is outside the guest supernet", p.GuestIP)
	}
	if transport.Contains(guestIP) {
		t.Fatalf("guest address collides with the transport pool")
	}
}

// TestPlanInstanceAddressesTransportExhaustion ensures a tiny transport pool
// fails loudly instead of silently reusing addresses.
func TestPlanInstanceAddressesTransportExhaustion(t *testing.T) {
	_, supernet, _ := net.ParseCIDR("10.0.0.0/8") // room for plenty of /30s
	_, transport, _ := net.ParseCIDR("169.254.0.0/30")
	if _, err := planInstanceAddresses(supernet, transport, 1); err == nil {
		t.Fatal("expected transport pool exhaustion error")
	}
}

// TestReserveExhaustion checks the index allocator reports exhaustion for a
// single-subnet pool. It skips if the probe interfaces happen to exist already.
func TestReserveExhaustion(t *testing.T) {
	if indexInUse(0) {
		t.Skip("vh0/vg0 already present on this host; skipping")
	}

	m, err := NewNetnsManager("10.9.9.0/30") // exactly one /30
	if err != nil {
		t.Fatalf("NewNetnsManager: %v", err)
	}
	idx, err := m.reserve()
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if idx != 0 {
		t.Fatalf("first reserve idx = %d, want 0", idx)
	}
	if _, err := m.reserve(); err == nil {
		t.Fatal("expected pool exhaustion on the second reserve")
	}
}
