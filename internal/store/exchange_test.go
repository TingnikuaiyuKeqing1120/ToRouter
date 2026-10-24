package store

import (
	"testing"
	"time"
)

func TestExchangeAttemptSurvivesInterruptedFetch(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected simulated interruption")
			}
		}()
		s.DailyExchangeRate(now, false, func() (ExchangeRate, error) { panic("process interrupted after persisted claim") })
	}()
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.DailyExchangeRate(now, false, func() (ExchangeRate, error) {
		t.Fatal("interrupted day retried after restart")
		return ExchangeRate{}, nil
	})
	if err != nil || v.Available || !v.Stale || v.Error == "" {
		t.Fatal(v, err)
	}
}
