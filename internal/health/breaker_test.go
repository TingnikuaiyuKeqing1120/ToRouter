package health

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerCooldownProbeAndRecovery(t *testing.T) {
	b := New()
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	k := Key{ChannelID: 1, Model: "m", CredentialID: "a"}
	for n, want := range []time.Duration{10, 20, 40, 80, 160, 300, 300} {
		l, ok := b.Acquire(k)
		if !ok {
			t.Fatal("probe unavailable")
		}
		b.Failure(l)
		v := b.Snapshot()[k]
		if v.Failures != n+1 || !v.Red || v.Until.Sub(now) != want*time.Second {
			t.Fatalf("failure %d: %+v", n+1, v)
		}
		if _, ok := b.Acquire(k); ok {
			t.Fatal("must cool down from first failure")
		}
		now = now.Add(want * time.Second)
		if v := b.Snapshot()[k]; v.Red != (n+1 >= 4) || v.Failures != n+1 {
			t.Fatalf("expiry cleared count or wrong color: %+v", v)
		}
	}
	var winners atomic.Int32
	var winner Lease
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l, ok := b.Acquire(k); ok {
				winner = l
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners=%d", winners.Load())
	}
	b.Success(winner)
	if v := b.Snapshot()[k]; v.Failures != 0 || v.Red || v.Probing {
		t.Fatal(v)
	}
	if _, ok := b.Acquire(k); !ok {
		t.Fatal("not recovered")
	}
}
func TestReleaseProbe(t *testing.T) {
	b := New()
	k := Key{ChannelID: 2}
	l, _ := b.Acquire(k)
	b.Failure(l.WithRetryAfter(0))
	probe, ok := b.Acquire(k)
	if !ok {
		t.Fatal("probe unavailable")
	}
	b.Release(probe)
	if _, ok := b.Acquire(k); !ok {
		t.Fatal("probe stuck")
	}
	if b.Snapshot()[k].Failures != 1 {
		t.Fatal("release cleared count")
	}
}
func TestRetryAfterAndAttemptConfiguration(t *testing.T) {
	b := New()
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	k := Key{ChannelID: 1}
	l, _ := b.Acquire(k)
	b.SetCooldown(time.Second)
	b.Failure(l)
	if b.Remaining(k) != 10*time.Second {
		t.Fatal("old attempt used new setting")
	}
	now = now.Add(10 * time.Second)
	l, _ = b.Acquire(k)
	b.Failure(l.WithRetryAfter(time.Hour))
	if b.Remaining(k) != time.Hour {
		t.Fatal("Retry-After truncated")
	}
	now = now.Add(time.Hour)
	l, _ = b.Acquire(k)
	b.Failure(l.WithRetryAfter(0))
	if b.Remaining(k) != 0 {
		t.Fatal("local cooldown added to Retry-After")
	}
}
func TestCombinationIsolationAndReset(t *testing.T) {
	b := New()
	base := Key{ChannelID: 1, Generation: 1, Endpoint: "https://example.test/v1", Format: "chat", Model: "m", CredentialID: "a"}
	variants := []Key{base, base, base, base, base}
	variants[0].Model = "other"
	variants[1].CredentialID = "b"
	variants[2].Format = "responses"
	variants[3].Generation = 2
	variants[4].Endpoint = "https://other.test"
	old, _ := b.Acquire(base)
	b.Failure(old)
	for _, k := range variants {
		if _, ok := b.Acquire(k); !ok {
			t.Fatalf("unrelated combination blocked: %+v", k)
		}
	}
	b.Reset(1, "a", "m")
	b.Failure(old)
	b.Observe(old, LastRequest{At: time.Now(), Status: 503})
	b.Success(old)
	if v := b.Snapshot()[base]; v.Failures != 0 || v.Red || v.LastRequest != nil {
		t.Fatalf("old receipt contaminated reset: %+v", v)
	}
}

func TestReconcileRecreatedKeyRejectsOldLeaseWithoutTombstone(t *testing.T) {
	b := New()
	key := Key{ChannelID: 1, Generation: 1, CredentialID: "a", Model: "m"}
	old, _ := b.Acquire(key)
	b.Reconcile(func(Key) bool { return false })
	if _, ok := b.Acquire(key); ok {
		t.Fatal("removed key was recreated by stale request")
	}
	b.Reconcile(func(k Key) bool { return k == key })
	current, ok := b.Acquire(key)
	if !ok {
		t.Fatal("current key unavailable")
	}
	if b.Failure(old) || b.Success(old) {
		t.Fatal("old receipt matched recreated key")
	}
	b.Observe(old, LastRequest{At: time.Now(), Status: 500})
	if v := b.Snapshot()[key]; v.Failures != 0 || v.LastRequest != nil {
		t.Fatalf("stale receipt mutated current state: %+v", v)
	}
	if !b.Failure(current) || b.Snapshot()[key].Failures != 1 {
		t.Fatal("current lease stopped working")
	}
}

func TestAttemptLocalCooldownDoesNotRaceSettings(t *testing.T) {
	b := New()
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	a := Key{ChannelID: 1}
	c := Key{ChannelID: 2}
	first, _ := b.Acquire(a)
	second, _ := b.Acquire(c)
	first = first.WithCooldown(time.Second)
	second = second.WithCooldown(20 * time.Second)
	b.SetCooldown(300 * time.Second)
	b.Failure(second)
	b.Failure(first)
	if b.Remaining(a) != time.Second || b.Remaining(c) != 20*time.Second {
		t.Fatal("one request's settings changed another lease")
	}
}
