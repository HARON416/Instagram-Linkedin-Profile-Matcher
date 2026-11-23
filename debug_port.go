package main

import (
	"fmt"
	"hash/fnv"
	"net"
)

// A stable nonzero port per session directory, independent of pool scheduling.
func browserDebugPort(directory string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(directory))
	const minimum, maximum = 9222, 65535
	return minimum + int(hash.Sum32()%uint32(maximum-minimum+1))
}

func checkBrowserDebugPort(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("browser debugging port %d is unavailable; close the existing browser using it or choose another session root: %w", port, err)
	}
	return listener.Close()
}
