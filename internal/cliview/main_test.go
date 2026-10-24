package cliview

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torouter/internal/app"
	"torouter/internal/store"
)

func TestBannerRedactsHomePaths(t *testing.T) {
	for _, dir := range []string{`C:\Users\Example User\to-router`, `/home/fixture-user/.config/to-router`} {
		for _, attached := range []bool{false, true} {
			var out bytes.Buffer
			writeBanner(&out, "http://127.0.0.1:12345", dir, attached)
			text := out.String()
			if strings.Contains(text, "Example User") || strings.Contains(text, "fixture-user") || strings.Count(text, "[HOME]") != 2 {
				t.Fatalf("banner leaked home identity: %s", text)
			}
			if !strings.Contains(text, "http://127.0.0.1:12345") || !strings.Contains(text, "to-router.log") {
				t.Fatalf("banner lost diagnostic content: %s", text)
			}
		}
	}
}

func TestStreamLogFileRedactsHistoricalContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.log")
	adminKey := "tr_" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	original := "INFO data=C:\\Users\\fixture-user\\to-router detail=tr_mcp_Synthetic-private-123 admin=" + adminKey + "\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	oldStdout := os.Stdout
	t.Cleanup(func() { os.Stdout = oldStdout })
	os.Stdout = writer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = streamLogFile(ctx, path)
	_ = writer.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "Synthetic") || strings.Contains(text, "fixture-user") || strings.Contains(text, adminKey) || !strings.Contains(text, `[HOME]\to-router`) || strings.Count(text, "[REDACTED]") != 2 {
		t.Fatalf("historical log not safely displayed: %s", text)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != original {
		t.Fatal("display redaction changed the underlying log file")
	}
}

func TestCLIFlagOutputRedactsCompleteUsageAndParseErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"help", []string{"-h"}, "Usage of [HOME]"},
		{"invalid value", []string{"-port", `C:\Users\fixture-user\invalid-port`}, "invalid value"},
		{"unknown flag", []string{`-C:\Users\fixture-user\unknown`}, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("USERPROFILE", `C:\Users\fixture-user`)
			t.Setenv("HOME", `/home/fixture-user`)
			oldArgs, oldFlags := os.Args, flag.CommandLine
			t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })
			os.Args = append([]string{`C:\Users\fixture-user\bin\torouter-cli.exe`}, tc.args...)
			flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
			var out bytes.Buffer
			flag.CommandLine.SetOutput(&out)
			err := run(os.Args[1:], flag.CommandLine)
			if tc.name == "help" && err != nil {
				t.Fatalf("help should exit successfully: %v", err)
			}
			if tc.name != "help" && (err == nil || errors.Is(err, flag.ErrHelp)) {
				t.Fatal("invalid arguments should return their parsing error")
			}
			text := out.String()
			if strings.Contains(text, "fixture-user") || !strings.Contains(text, tc.want) || !strings.Contains(text, "-show-key") || !strings.Contains(text, "-data") {
				t.Fatalf("complete flag output leaked identity or lost usage: %s", text)
			}
		})
	}
}

func TestShowKeyStillPrintsExplicitSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", `C:\Users\fixture-user`)
	t.Setenv("HOME", `/home/fixture-user`)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := app.ReadAdminKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	oldArgs, oldFlags, oldStdout := os.Args, flag.CommandLine, os.Stdout
	t.Cleanup(func() { os.Args, flag.CommandLine, os.Stdout = oldArgs, oldFlags, oldStdout })
	os.Args = []string{"torouter-cli", "-show-key", "-data", dir}
	flag.CommandLine = flag.NewFlagSet("show-key-test", flag.ContinueOnError)
	os.Stdout = writer
	err = run(os.Args[1:], flag.CommandLine)
	_ = writer.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want+"\n" {
		t.Fatalf("-show-key did not preserve explicit disclosure")
	}
	var help bytes.Buffer
	flag.CommandLine.SetOutput(&help)
	flag.CommandLine.PrintDefaults()
	if strings.Contains(help.String(), "fixture-user") || !strings.Contains(help.String(), "[HOME]") {
		t.Fatal("flag help leaked default home identity")
	}
	if flag.Lookup("data").Value.String() != dir {
		t.Fatal("display redaction changed the actual data directory")
	}
}
