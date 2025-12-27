package network

import (
	"encoding/binary"
	"fmt"
	"net"
)

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip)
}

func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}

func allocateIP(subnet *net.IPNet, usedIPs map[string]bool) (string, error) {
	networkIP := subnet.IP.To4()
	if networkIP == nil {
		return "", fmt.Errorf("only IPv4 is supported")
	}

	maskOnes, maskBits := subnet.Mask.Size()
	if maskBits != 32 {
		return "", fmt.Errorf("only IPv4 is supported")
	}

	numHosts := uint32(1<<(32-maskOnes)) - 2
	startIP := ipToUint32(networkIP) + 2

	for i := uint32(0); i < numHosts-1; i++ {
		candidateIP := uint32ToIP(startIP + i)
		candidateStr := candidateIP.String()

		if !usedIPs[candidateStr] && subnet.Contains(candidateIP) {
			return candidateStr, nil
		}
	}

	return "", fmt.Errorf("no available IPs in subnet %s", subnet.String())
}

