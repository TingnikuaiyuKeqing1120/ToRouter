// Package companion observes request lifecycles for the authenticated HUD.
// It never selects channels, changes retries, or retains request/response text.
package companion

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type LiveRequest struct {
	ID                 string    `json:"id"`
	Model              string    `json:"model"`
	Channel            string    `json:"channel"`
	ChannelID          int64     `json:"channel_id"`
	StartedAt          time.Time `json:"started_at"`
	Attempt            int       `json:"attempt"`
	Phase              string    `json:"phase"`
	OutputTokens       *int64    `json:"output_tokens"`
	OutputTokensSource string    `json:"output_tokens_source"`
	TokensPerSecond    *float64  `json:"tokens_per_second"`
	FirstByteMS        *int64    `json:"first_byte_ms"`
}

type FinishedRequest struct {
	LiveRequest
	Sequence   uint64    `json:"sequence"`
	FinishedAt time.Time `json:"finished_at"`
	Outcome    string    `json:"outcome"`
	Status     int       `json:"status"`
	ErrorCode  string    `json:"error_code"`
}

type Snapshot struct {
	ServerTime  time.Time         `json:"server_time"`
	SessionID   string            `json:"session_id"`
	Sequence    uint64            `json:"sequence"`
	ActiveCount int               `json:"active_count"`
	Active      []LiveRequest     `json:"active"`
	Recent      []FinishedRequest `json:"recent"`
}

type entry struct {
	LiveRequest
	stream         bool
	firstByte      time.Time
	ordinal        uint64
	reportedTokens int64
	textUnits      int64
}

type Tracker struct {
	mu       sync.Mutex
	session  string
	next     uint64
	sequence uint64
	active   map[string]*entry
	recent   []FinishedRequest
	now      func() time.Time
}

func New() *Tracker {
	return &Tracker{session: uuid.NewString(), active: make(map[string]*entry), now: time.Now}
}

type Request struct {
	tracker *Tracker
	id      string
}

func (t *Tracker) Begin(model string, stream bool, started time.Time) *Request {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	id := strconv.FormatUint(t.next, 10)
	t.active[id] = &entry{LiveRequest: LiveRequest{ID: id, Model: publicName(model), StartedAt: started, Phase: "waiting"}, stream: stream, ordinal: t.next}
	return &Request{tracker: t, id: id}
}

func publicName(value string) string {
	runes := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value))
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return string(runes)
}

func (r *Request) update(fn func(*entry, time.Time)) {
	if r == nil {
		return
	}
	t := r.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.active[r.id]; e != nil {
		fn(e, t.now())
	}
}

func (r *Request) Attempt(channelID int64, channel string) {
	r.update(func(e *entry, _ time.Time) {
		e.ChannelID, e.Channel = channelID, publicName(channel)
		e.Attempt++
		e.OutputTokens, e.FirstByteMS, e.TokensPerSecond = nil, nil, nil
		e.OutputTokensSource = ""
		e.reportedTokens, e.textUnits = 0, 0
		e.firstByte = time.Time{}
		e.Phase = "waiting"
		if e.Attempt > 1 {
			e.Phase = "retrying"
		}
	})
}

func (r *Request) Retrying() { r.update(func(e *entry, _ time.Time) { e.Phase = "retrying" }) }

// FirstByte is called only after a successful response-body write. This is a
// first-byte latency, not an invented first-content-token measurement.
func (r *Request) FirstByte() {
	r.update(func(e *entry, now time.Time) {
		if e.firstByte.IsZero() {
			e.firstByte = now
			ms := max(int64(0), now.Sub(e.StartedAt).Milliseconds())
			e.FirstByteMS = &ms
		}
		e.Phase = "streaming"
	})
}

// Output accepts an upstream-reported cumulative output count and calibrates
// the estimate, including when the reported count is lower than the estimate.
func (r *Request) Output(tokens int64) {
	if tokens < 0 {
		return
	}
	r.update(func(e *entry, _ time.Time) {
		e.reportedTokens, e.textUnits = tokens, 0
		n := tokens
		e.OutputTokens = &n
		e.OutputTokensSource = "reported"
	})
}

// TextUnits observes incremental output without retaining text: ASCII code
// points contribute one quarter-token unit, non-ASCII code points four. Units
// accumulate between usage reports so splitting text cannot change the total.
func (r *Request) TextUnits(units int64) {
	if units <= 0 {
		return
	}
	r.update(func(e *entry, _ time.Time) {
		if !e.stream {
			return
		}
		e.textUnits += units
		n := e.reportedTokens + (e.textUnits+3)/4
		e.OutputTokens = &n
		e.OutputTokensSource = "estimated"
	})
}

func snapshotEntry(e *entry, now time.Time) LiveRequest {
	v := e.LiveRequest
	if v.OutputTokens != nil {
		n := *v.OutputTokens
		v.OutputTokens = &n
	}
	if v.FirstByteMS != nil {
		n := *v.FirstByteMS
		v.FirstByteMS = &n
	}
	v.TokensPerSecond = nil
	// Non-streamed responses do not expose generation time. A final JSON body
	// appearing all at once cannot justify a token generation speed.
	if e.stream && v.OutputTokens != nil && !e.firstByte.IsZero() {
		seconds := now.Sub(e.firstByte).Seconds()
		if seconds >= .05 {
			rate := float64(*v.OutputTokens) / seconds
			v.TokensPerSecond = &rate
		}
	}
	return v
}

func (r *Request) Finish(status int, outcome, code string) {
	if r == nil {
		return
	}
	t := r.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.active[r.id]
	if e == nil {
		return
	}
	now := t.now()
	t.sequence++
	if outcome != "success" && outcome != "cancelled" && outcome != "accepted" {
		outcome = "error"
	}
	if outcome == "success" || outcome == "accepted" {
		code = ""
	} else {
		code = safeCode(code)
	}
	t.recent = append(t.recent, FinishedRequest{LiveRequest: snapshotEntry(e, now), Sequence: t.sequence, FinishedAt: now, Outcome: outcome, Status: status, ErrorCode: code})
	if len(t.recent) > 32 {
		t.recent = append([]FinishedRequest(nil), t.recent[len(t.recent)-32:]...)
	}
	delete(t.active, r.id)
}

func safeCode(code string) string {
	switch code {
	case "request_rejected", "no_usable_endpoint", "all_channels_cooling", "upstream_error", "stream_interrupted", "response_incomplete", "configuration_error", "client_cancelled", "conversion_refused", "state_affinity_conflict", "state_domain_changed", "state_affinity_unavailable", "state_affinity_unknown", "upstream_may_have_processed":
		return code
	default:
		return "upstream_error"
	}
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := Snapshot{ServerTime: now, SessionID: t.session, Sequence: t.sequence, ActiveCount: len(t.active), Active: []LiveRequest{}, Recent: []FinishedRequest{}}
	ordered := make([]*entry, 0, len(t.active))
	for _, e := range t.active {
		ordered = append(ordered, e)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ordinal < ordered[j].ordinal })
	for i, e := range ordered {
		if i >= 16 {
			break
		}
		s.Active = append(s.Active, snapshotEntry(e, now))
	}
	for _, e := range t.recent {
		if now.Sub(e.FinishedAt) <= 30*time.Second {
			s.Recent = append(s.Recent, e)
		}
	}
	return s
}
