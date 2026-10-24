package store

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

// Two concurrent edits of one channel must not both read the same generation
// and each write its successor: a repeated generation re-uses a continuation
// domain, so a session pinned against one editor's credential would be served
// by the other's. The read→compare→increment→write is one critical section, so
// the saved generations come out consecutive and never repeat.
func TestConcurrentSaveChannelKeepsGenerationsDistinct(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const editors, rounds = 8, 20
	for round := 0; round < rounds; round++ {
		base := Channel{Name: fmt.Sprintf("concurrent-%d", round), APIKey: "key-0", Models: []string{"m"}, Enabled: true,
			Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "http://localhost:1/v1"}}}
		if err = s.SaveChannel(&base); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		got := make([]int64, editors)
		for i := 0; i < editors; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// Every editor waits for the same gate, so the reads of the
				// stored row all land before any of the writes.
				<-start
				c := base
				c.APIKey = fmt.Sprintf("key-%d-%d", round, i)
				if err := s.SaveChannel(&c); err != nil {
					t.Error(err)
					return
				}
				got[i] = c.Generation
			}(i)
		}
		close(start)
		wg.Wait()
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		for i, gen := range got {
			if want := base.Generation + int64(i) + 1; gen != want {
				t.Fatalf("round %d: saved generations = %v, want %d..%d without repeats", round, got, base.Generation+1, base.Generation+editors)
			}
		}
		stored, err := s.channel(base.ID)
		if err != nil {
			t.Fatalf("round %d: channel(%d) = %v", round, base.ID, err)
		}
		if stored.Generation != base.Generation+editors {
			t.Fatalf("round %d: stored generation = %d, want %d", round, stored.Generation, base.Generation+editors)
		}
	}
}
