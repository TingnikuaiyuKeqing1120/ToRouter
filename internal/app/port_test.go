package app

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListenBindsWantedPort(t *testing.T) {
	listener, err := Listen(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if listener.Addr() == nil {
		t.Fatal("no address")
	}
}

func TestListenRefusesOccupiedPortWithoutFallback(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	taken := blocker.Addr().(*net.TCPAddr).Port

	_, listenErr := Listen(taken, 0)
	if listenErr == nil {
		t.Fatal("must not silently move to another port")
	}
	// The error must name the port and the remedies, because this is the only
	// channel the user has when the web console cannot start either.
	message := listenErr.Error()
	for _, want := range []string{strconv.Itoa(taken), "-port"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error missing %q: %s", want, message)
		}
	}
}

func TestListenWaitsForPredecessorWithinGrace(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	taken := blocker.Addr().(*net.TCPAddr).Port
	go func() {
		time.Sleep(300 * time.Millisecond)
		blocker.Close()
	}()
	listener, err := Listen(taken, 5*time.Second)
	if err != nil {
		t.Fatalf("should have waited for the port: %v", err)
	}
	defer listener.Close()
	if bound := listener.Addr().(*net.TCPAddr).Port; bound != taken {
		t.Fatalf("bound %d instead of %d", bound, taken)
	}
}

func TestListenGivesUpAfterGrace(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	taken := blocker.Addr().(*net.TCPAddr).Port
	start := time.Now()
	if _, err := Listen(taken, 500*time.Millisecond); err == nil {
		t.Fatal("expected failure once the grace period elapsed")
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("gave up too early: %v", elapsed)
	}
}
