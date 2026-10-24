//go:build windows

package desktopui

import (
	"testing"
	"time"
)

func TestPetLaunchGateSerializesAndReleases(t *testing.T) {
	id := singletonID(t.TempDir())
	first, err := acquirePetLaunchGate(id)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	entered := make(chan func(), 1)
	failure := make(chan error, 1)
	go func() {
		release, err := acquirePetLaunchGate(id)
		if err != nil {
			failure <- err
			return
		}
		entered <- release
	}()
	select {
	case second := <-entered:
		second()
		t.Fatal("racing launch entered before first message window was ready")
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(75 * time.Millisecond):
	}
	first()
	first() // release is idempotent across startup and run's defer.
	select {
	case second := <-entered:
		second()
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("released gate did not admit next launch")
	}
	third, err := acquirePetLaunchGate(id)
	if err != nil {
		t.Fatal(err)
	}
	third()
}
