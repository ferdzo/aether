package id

import (
	"os"
	"strings"

	"github.com/rs/xid"
)

func GenerateVMID() string {
	return "vm-" + xid.New().String()
}

func GenerateRequestID() string {
	return "req-" + xid.New().String()
}

func GetWorkerID() string {
	if id := os.Getenv("AETHER_WORKER_ID"); id != "" {
		return cleanID(id)
	}

	if data, err := os.ReadFile("/etc/machine-id"); err == nil {
		machineID := strings.TrimSpace(string(data))
		if len(machineID) >= 12 {
			return "worker-" + machineID[:12]
		}
	}

	return "worker-" + xid.New().String()
}

func cleanID(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "_", "-")
	return s
}