//go:build windows

package launch

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestConsoleRedirectHelper(t *testing.T) {
	mode := os.Getenv("TOROUTER_CONSOLE_TEST")
	if mode == "" {
		return
	}
	stdout, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	stderr, _ := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err := PrepareConsole(); err != nil {
		t.Fatal(err)
	}
	afterOut, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	afterErr, _ := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if stdout != afterOut || stderr != afterErr {
		t.Fatal("redirected output handles replaced")
	}
	if window, _, _ := kernel.NewProc("GetConsoleWindow").Call(); window != 0 {
		t.Fatal("redirected CLI created a visible console")
	}
	if mode == "pipe" {
		input, err := io.ReadAll(os.Stdin)
		if err != nil || string(input) != "synthetic-input" {
			t.Fatalf("stdin pipe changed: %v", err)
		}
	}
	fmt.Fprintln(os.Stdout, "redirected-stdout")
	fmt.Fprintln(os.Stderr, "redirected-stderr")
}

func TestConsolePreservesPipesWithoutAllocatingConsole(t *testing.T) {
	for _, input := range []string{"pipe", "null"} {
		t.Run(input, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestConsoleRedirectHelper$")
			command.Env = append(os.Environ(), "TOROUTER_CONSOLE_TEST="+input)
			command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
			if input == "pipe" {
				command.Stdin = strings.NewReader("synthetic-input")
			}
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err != nil {
				t.Fatalf("helper: %v, %s %s", err, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "redirected-stdout") || !strings.Contains(stderr.String(), "redirected-stderr") {
				t.Fatal("redirected stream was lost")
			}
		})
	}
}
