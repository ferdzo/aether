package system

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

type Info struct {
	CPUCount  int
	MemoryMB  int
	Hostname  string
	MachineID string
}

func GetInfo() Info {
	info := Info{
		CPUCount: runtime.NumCPU(),
		MemoryMB: getMemoryMB(),
	}

	info.Hostname, _ = os.Hostname()

	if data, err := os.ReadFile("/etc/machine-id"); err == nil {
		info.MachineID = strings.TrimSpace(string(data))
	}

	return info
}

func getMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.Atoi(fields[1])
				return kb / 1024 // Convert KB to MB
			}
		}
	}

	return 0
}

func GetFreeRAM() (int, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.Atoi(fields[1])
				return kb / 1024, nil // Convert KB to MB
			}
		}
	}

	return 0, nil
}
