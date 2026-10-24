package health

import (
	"sync"
	"time"
)

// Key names an actual outbound combination. Inbound protocols deliberately do
// not participate: two listeners using the same upstream share its health.
type Key struct {
	ChannelID    int64
	Generation   int64
	Endpoint     string
	Format       string
	Model        string
	CredentialID string
}
type LastRequest struct {
	At         time.Time `json:"at"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
}
type State struct {
	Failures    int
	Until       time.Time
	Probing     bool
	Red         bool
	LastRequest *LastRequest
}
type state struct {
	failures int
	until    time.Time
	probe    uint64
	epoch    uint64
	last     *LastRequest
}
type Breaker struct {
	mu        sync.Mutex
	states    map[Key]*state
	nextProbe uint64
	nextEpoch uint64
	valid     func(Key) bool
	cooldown  time.Duration
	now       func() time.Time
}
type Lease struct {
	key        Key
	probe      uint64
	epoch      uint64
	retryAfter *time.Duration
	cooldown   time.Duration
}

func (l Lease) Key() Key                             { return l.key }
func (l Lease) WithRetryAfter(d time.Duration) Lease { l.retryAfter = &d; return l }
func New() *Breaker {
	return &Breaker{states: map[Key]*state{}, cooldown: 10 * time.Second, now: time.Now}
}
func (b *Breaker) SetCooldown(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d <= 0 {
		d = 10 * time.Second
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	b.cooldown = d
}
func (b *Breaker) Acquire(k Key) (Lease, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.valid != nil && !b.valid(k) {
		return Lease{}, false
	}
	s := b.states[k]
	if s == nil {
		b.nextEpoch++
		s = &state{epoch: b.nextEpoch}
		b.states[k] = s
	}
	if b.now().Before(s.until) || s.probe != 0 {
		return Lease{}, false
	}
	l := Lease{key: k, epoch: s.epoch, cooldown: b.cooldown}
	if s.failures > 0 {
		b.nextProbe++
		s.probe = b.nextProbe
		l.probe = s.probe
	}
	return l, true
}
func (b *Breaker) Success(l Lease) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[l.key]
	if s == nil || s.epoch != l.epoch || s.probe != l.probe {
		return false
	}
	s.failures = 0
	s.until = time.Time{}
	s.probe = 0
	// Do not invalidate this attempt's subsequent request metadata. Concurrent
	// pre-success receipts are still from this live configuration/reset epoch.
	return true
}
func (b *Breaker) Release(l Lease) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.states[l.key]; s != nil && s.epoch == l.epoch && s.probe == l.probe {
		s.probe = 0
	}
}
func (b *Breaker) Failure(l Lease) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[l.key]
	if s == nil || s.epoch != l.epoch || (l.probe != 0 && s.probe != l.probe) {
		return false
	}
	s.failures++
	if s.probe == l.probe {
		s.probe = 0
	}
	d := l.cooldown
	for i := 1; i < s.failures && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	if l.retryAfter != nil {
		d = *l.retryAfter
	}
	s.until = b.now().Add(d)
	return true
}
func (b *Breaker) Observe(l Lease, request LastRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.states[l.key]; s != nil && s.epoch == l.epoch && (s.last == nil || !request.At.Before(s.last.At)) {
		s.last = &request
	}
}
func (b *Breaker) Available(k Key) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[k]
	return s == nil || (!b.now().Before(s.until) && s.probe == 0)
}
func (b *Breaker) Remaining(k Key) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.states[k]; s != nil {
		return max(0, s.until.Sub(b.now()))
	}
	return 0
}
func (b *Breaker) Reset(channelID int64, credentialID, model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, s := range b.states {
		if k.ChannelID == channelID && k.CredentialID == credentialID && (model == "" || k.Model == model) {
			b.nextEpoch++
			s.epoch = b.nextEpoch
			s.failures = 0
			s.until = time.Time{}
			s.probe = 0
			s.last = nil
		}
	}
}
func (b *Breaker) Snapshot() map[Key]State {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[Key]State{}
	for k, s := range b.states {
		v := State{Failures: s.failures, Until: s.until, Probing: s.probe != 0, Red: s.failures >= 4 || b.now().Before(s.until)}
		if s.last != nil {
			copy := *s.last
			v.LastRequest = &copy
		}
		out[k] = v
	}
	return out
}

// Reconcile drops only combinations removed from current configuration. Live
// current attempts retain their state and lease epoch. No tombstones are kept;
// a recreated key receives a globally fresh epoch in Acquire.
func (b *Breaker) Reconcile(valid func(Key) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.valid = valid
	for k := range b.states {
		if !valid(k) {
			delete(b.states, k)
		}
	}
}

// WithCooldown binds request-local settings to this attempt. Another request
// observing newer settings cannot change this attempt's eventual failure delay.
func (l Lease) WithCooldown(d time.Duration) Lease {
	if d <= 0 {
		d = 10 * time.Second
	}
	l.cooldown = min(d, 5*time.Minute)
	return l
}
