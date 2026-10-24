package quota

import "time"

// Observation is an account availability fact. Nil Available means unknown;
// it must never be treated as an exhausted balance or a failed model call.
type Observation struct {
	ChannelID    int64
	Generation   int64
	CredentialID string
	Supported    bool
	Available    *bool
	Reason       string
	CheckedAt    time.Time
	ExpiresAt    time.Time
	Error        string
	Balances     []Balance
}

type Balance struct {
	Currency string `json:"currency"`
	Total    string `json:"total"`
}

type Window struct {
	ID               string     `json:"id"`
	Label            string     `json:"label"`
	RemainingPercent *float64   `json:"remaining_percent"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
}

// Snapshot is the shared local display projection. It contains neither
// account identity nor credential material; amounts are exact decimals.
type Snapshot struct {
	ChannelID      int64      `json:"channel_id"`
	CredentialID   string     `json:"credential_id"`
	ChannelName    string     `json:"channel_name"`
	CredentialName string     `json:"credential_name"`
	Kind           string     `json:"kind"`
	Enabled        bool       `json:"enabled"`
	Balances       []Balance  `json:"balances"`
	Windows        []Window   `json:"windows"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	NextRefreshAt  *time.Time `json:"next_refresh_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	Stale          bool       `json:"stale"`
}
