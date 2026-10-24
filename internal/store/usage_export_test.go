package store

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type failingUsageWriter struct{}

func (failingUsageWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

func TestUsageCSVFailureAndCancellation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ExportUsageCSV(context.Background(), UsageFilter{}, failingUsageWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write failure lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ExportUsageCSV(ctx, UsageFilter{}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled export: %v", err)
	}
	if err := s.AddUsage(Usage{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,0,'broken',200,?)", time.Now().UnixMilli(), "{invalid-json}"); err != nil {
		t.Fatal(err)
	}
	if err := s.ExportUsageCSV(context.Background(), UsageFilter{}, io.Discard); err == nil {
		t.Fatal("read failure ignored")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		t.Fatalf("export leaked connection: %v", err)
	}
}

func TestUsageCSVFormulaGuard(t *testing.T) {
	for _, value := range []string{"=1+2", "+1", "-2", "@A1", " \t=1", "\tplain", "\nplain", "\rplain", "\u2003=1"} {
		if got := csvUsageText(value); got != "'"+value {
			t.Errorf("formula not escaped: %q -> %q", value, got)
		}
	}
	for _, value := range []string{"", "model-name", "渠道", "a,b\nquoted", "1+2"} {
		if got := csvUsageText(value); got != value {
			t.Errorf("ordinary text modified: %q -> %q", value, got)
		}
	}
	if csvUsageAmount(nil) != "" {
		t.Fatal("unknown amount must stay empty")
	}
}
