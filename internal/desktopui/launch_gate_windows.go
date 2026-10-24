//go:build windows

package desktopui

import (
	"fmt"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

// Wails v2 creates its named mutex before its message window. During that gap
// a second launch can see the mutex but no window and incorrectly start again.
// This short gate covers only Wails startup. A locked OS thread owns/releases
// the mutex, including when OnStartup runs on a different goroutine.
func acquirePetLaunchGate(id string) (func(), error) {
	acquired := make(chan error, 1)
	release := make(chan struct{})
	released := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(released)
		name, err := windows.UTF16PtrFromString("Local\\" + id + "-launch")
		if err != nil {
			acquired <- err
			return
		}
		handle, err := windows.CreateMutex(nil, false, name)
		if err != nil && err != windows.ERROR_ALREADY_EXISTS {
			acquired <- err
			return
		}
		defer windows.CloseHandle(handle)
		state, err := windows.WaitForSingleObject(handle, 30000)
		if err != nil {
			acquired <- err
			return
		}
		if state != windows.WAIT_OBJECT_0 && state != windows.WAIT_ABANDONED {
			acquired <- fmt.Errorf("desktop companion startup busy (wait %d)", state)
			return
		}
		acquired <- nil
		<-release
		windows.ReleaseMutex(handle)
	}()
	if err := <-acquired; err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { close(release); <-released }) }, nil
}
