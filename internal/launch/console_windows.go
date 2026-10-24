//go:build windows

// Package launch provides process setup shared by the executable's modes.
package launch

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var kernel = windows.NewLazySystemDLL("kernel32.dll")

// PrepareConsole preserves redirected streams, attaches the launching terminal
// when available, and creates a console only for an interactive CLI launch.
func PrepareConsole() error {
	streams := []struct {
		id   uint32
		file **os.File
		name string
	}{
		{windows.STD_INPUT_HANDLE, &os.Stdin, "stdin"},
		{windows.STD_OUTPUT_HANDLE, &os.Stdout, "stdout"},
		{windows.STD_ERROR_HANDLE, &os.Stderr, "stderr"},
	}
	preserved := make(map[uint32]windows.Handle)
	hasConsole := false
	for _, stream := range streams {
		h, err := windows.GetStdHandle(stream.id)
		if err != nil || h == 0 || h == windows.InvalidHandle {
			continue
		}
		kind, err := windows.GetFileType(h)
		if err == nil && (kind == windows.FILE_TYPE_PIPE || kind == windows.FILE_TYPE_DISK) {
			preserved[stream.id] = h
		}
		var mode uint32
		if windows.GetConsoleMode(h, &mode) == nil {
			hasConsole = true
		}
	}
	// Redirecting output makes this a command-line automation invocation even
	// when stdin is NUL. Never allocate a visible console for such commands.
	_, stdoutRedirected := preserved[windows.STD_OUTPUT_HANDLE]
	_, stderrRedirected := preserved[windows.STD_ERROR_HANDLE]
	if !hasConsole && !stdoutRedirected && !stderrRedirected {
		attached, _, _ := kernel.NewProc("AttachConsole").Call(^uintptr(0)) // ATTACH_PARENT_PROCESS
		if attached == 0 {
			allocated, _, err := kernel.NewProc("AllocConsole").Call()
			if allocated == 0 {
				return fmt.Errorf("create CLI console: %w", err)
			}
		}
	}
	for _, stream := range streams {
		h, keep := preserved[stream.id]
		if keep {
			if err := windows.SetStdHandle(stream.id, h); err != nil {
				return err
			}
			continue
		}
		h, err := windows.GetStdHandle(stream.id)
		if err != nil {
			return err
		}
		if h != 0 && h != windows.InvalidHandle {
			*stream.file = os.NewFile(uintptr(h), stream.name)
		}
	}
	return nil
}
