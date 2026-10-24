package health

import "time"

type StatusSnapshot struct {
	Rows         []StatusRow `json:"rows"`
	ProblemCount int         `json:"problem_count"`
}
type StatusRow struct {
	ChannelID      int64         `json:"channel_id"`
	ChannelName    string        `json:"channel_name"`
	CredentialID   string        `json:"credential_id"`
	CredentialName string        `json:"credential_name"`
	Red            bool          `json:"red"`
	Reason         string        `json:"reason"`
	Failures       int           `json:"failures"`
	CooldownUntil  *time.Time    `json:"cooldown_until,omitempty"`
	QuotaSupported bool          `json:"quota_supported"`
	Models         []ModelStatus `json:"models"`
}
type ModelStatus struct {
	Model         string       `json:"model"`
	Name          string       `json:"name"`
	Upstream      string       `json:"upstream"`
	Red           bool         `json:"red"`
	Reason        string       `json:"reason"`
	Failures      int          `json:"failures"`
	CooldownUntil *time.Time   `json:"cooldown_until,omitempty"`
	LastRequest   *LastRequest `json:"last_request,omitempty"`
}
