package main

import (
	"net"
	"testing"
)

func TestDebugPortsAreStableDistinctAndNonzero(t *testing.T) {
	first := browserDebugPort("/sessions/browser-001")
	second := browserDebugPort("/sessions/browser-002")
	if first < 9222 || first > 65535 || first == second || first != browserDebugPort("/sessions/browser-001") {
		t.Fatalf("unexpected profile ports: %d, %d", first, second)
	}
}

func TestOccupiedDebugPortIsRejected(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := checkBrowserDebugPort(listener.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("occupied port was accepted")
	}
}
