package health

import (
	"testing"
	"time"
)

// A closed-breaker request can outlive the failures and cooldown of other
// requests. Its completion must not release a new half-open probe it never
// acquired. The same rule applies to a previous probe's late completion.
func TestOldRequestCannotClearCurrentProbe(t *testing.T) {
	for _, completion := range []string{"release", "success", "failure"} {
		for _, oldProbe := range []bool{false, true} {
			name := completion + "/ordinary"
			if oldProbe {
				name = completion + "/previous-probe"
			}
			t.Run(name, func(t *testing.T) {
				b := New()
				now := time.Unix(0, 0)
				b.now = func() time.Time { return now }
				old, ok := b.Acquire(Key{ChannelID: 1})
				if !ok {
					t.Fatal("ordinary request unavailable")
				}
				for i := 0; i < 1; i++ {
					lease, _ := b.Acquire(Key{ChannelID: 1})
					b.Failure(lease)
				}
				now = now.Add(time.Minute)
				if oldProbe {
					old, ok = b.Acquire(Key{ChannelID: 1})
					if !ok {
						t.Fatal("old probe unavailable")
					}
					b.Release(old)
				}
				current, ok := b.Acquire(Key{ChannelID: 1})
				_, another := b.Acquire(Key{ChannelID: 1})
				if !ok || another {
					t.Fatal("current probe must be the only holder")
				}
				switch completion {
				case "release":
					b.Release(old)
				case "success":
					b.Success(old)
				case "failure":
					b.Failure(old)
				}
				if !b.Snapshot()[Key{ChannelID: 1}].Probing {
					t.Fatalf("old %s cleared the current probe: %v", name, b.Snapshot())
				}
				if _, ok := b.Acquire(Key{ChannelID: 1}); ok {
					t.Fatal("a second simultaneous probe was admitted")
				}
				b.Release(current)
				// A stale failure can still count against channel-level health;
				// wait out its cooldown before observing that the holder's own
				// cancellation releases its slot.
				now = now.Add(10 * time.Minute)
				if _, ok := b.Acquire(Key{ChannelID: 1}); !ok {
					t.Fatal("current holder's cancellation did not free its slot")
				}
			})
		}
	}
}
