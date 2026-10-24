package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"torouter/internal/accessguard"

	"torouter/internal/store"
)

type ResetCredit struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	GrantedAt  *int64 `json:"granted_at,omitempty"`
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
	RedeemedAt *int64 `json:"redeemed_at,omitempty"`
	Available  bool   `json:"available"`
}

type ResetCredits struct {
	CapturedAt     time.Time     `json:"captured_at"`
	AvailableCount *int          `json:"available_count"`
	Credits        []ResetCredit `json:"credits"`
}

// Unknown means the POST might have been processed. Callers must not replay it
// automatically; the upstream has not established an idempotency guarantee.
type CredentialActionError struct {
	Unknown bool
	Message string
}

func (e *CredentialActionError) Error() string { return e.Message }

func actionError(message string, unknown bool) error {
	return &CredentialActionError{Unknown: unknown, Message: message}
}

func resetCreditCount(raw json.RawMessage) (*int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var count int
	if err := json.Unmarshal(raw, &count); err != nil || count < 0 {
		return nil, errors.New("invalid reset credit count")
	}
	return &count, nil
}

func resetCreditTime(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		// Unix milliseconds have thirteen digits for current dates.
		if number > 100_000_000_000 || number < -100_000_000_000 {
			number /= 1000
		}
		return &number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, errors.New("invalid reset credit time")
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil, errors.New("invalid reset credit time")
	}
	number = t.Unix()
	return &number, nil
}

func parseResetCredits(body []byte, captured time.Time) (ResetCredits, error) {
	var root usageObject
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return ResetCredits{}, errors.New("expected reset credits object")
	}
	if nested, ok := root["data"]; ok {
		root = nil
		if err := json.Unmarshal(nested, &root); err != nil || root == nil {
			return ResetCredits{}, errors.New("invalid reset credits data")
		}
	}
	out := ResetCredits{CapturedAt: captured, Credits: []ResetCredit{}}
	var err error
	out.AvailableCount, err = resetCreditCount(usageField(root, "available_count", "availableCount"))
	if err != nil {
		return ResetCredits{}, err
	}
	raw := usageField(root, "credits")
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var entries []usageObject
	if err := json.Unmarshal(raw, &entries); err != nil {
		return ResetCredits{}, errors.New("invalid reset credits list")
	}
	available := 0
	knownCount := true
	for _, entry := range entries {
		if entry == nil {
			return ResetCredits{}, errors.New("invalid reset credit")
		}
		credit := ResetCredit{}
		for _, field := range []struct {
			name string
			dst  *string
		}{{"id", &credit.ID}, {"type", &credit.Type}, {"status", &credit.Status}} {
			if v := usageField(entry, field.name); len(v) > 0 && string(v) != "null" {
				if err := json.Unmarshal(v, field.dst); err != nil {
					return ResetCredits{}, errors.New("invalid reset credit text")
				}
			}
		}
		for _, field := range []struct {
			names []string
			dst   **int64
		}{
			{[]string{"granted_at", "grantedAt"}, &credit.GrantedAt},
			{[]string{"expires_at", "expiresAt"}, &credit.ExpiresAt},
			{[]string{"redeemed_at", "redeemedAt"}, &credit.RedeemedAt},
		} {
			*field.dst, err = resetCreditTime(usageField(entry, field.names...))
			if err != nil {
				return ResetCredits{}, err
			}
		}
		credit.Available = credit.Status == "available" && credit.RedeemedAt == nil && (credit.ExpiresAt == nil || *credit.ExpiresAt > captured.Unix())
		if credit.Status != "available" && credit.Status != "redeemed" && credit.Status != "expired" {
			knownCount = false
		}
		if credit.Available {
			available++
		}
		out.Credits = append(out.Credits, credit)
	}
	if out.AvailableCount == nil && knownCount {
		out.AvailableCount = &available
	}
	return out, nil
}

func (eps Endpoints) resetCreditsURL() string {
	if eps.ResetCredits != "" {
		return eps.ResetCredits
	}
	return strings.TrimSuffix(strings.TrimRight(eps.Usage, "/"), "/usage") + "/rate-limit-reset-credits"
}

// actionCredentialLocked binds the selected pool member before and after token
// rotation. The caller retains the lifecycle lock until the action finishes.
func (svc *Service) actionCredentialLocked(ctx context.Context, c store.Channel, id string, write bool, model string) (string, string, error) {
	if id == "" {
		return "", "", errors.New("a Codex credential must be selected")
	}
	expected, err := usageCredential(c, id)
	if err != nil {
		return "", "", err
	}
	check := func(rotatedToken string) (store.Channel, *store.OAuthCredential, error) {
		current, err := svc.Store.Channel(c.ID)
		if err != nil || current.Generation != c.Generation {
			return current, nil, errors.New("Codex account changed before action")
		}
		oauth, err := usageCredential(current, id)
		matches := err == nil && sameUsageIdentity(expected, oauth)
		if rotatedToken != "" && err == nil {
			// The refresh just proved lineage for this exact member. Anonymous
			// accounts may rotate their only identity evidence (refresh token).
			matches = oauth.Provider == expected.Provider && oauth.ClientID == expected.ClientID && oauth.Account == expected.Account && oauth.Subject == expected.Subject && oauth.AccessToken == rotatedToken
		}
		if !matches {
			return current, nil, errors.New("Codex account changed before action")
		}
		if write {
			enabled := false
			for _, cred := range current.Credentials {
				if cred.ID == id {
					enabled = cred.Enabled
					break
				}
			}
			if !current.Enabled || !enabled {
				return current, nil, errors.New("Codex channel or credential is disabled")
			}
			if model != "" {
				found := false
				for _, m := range current.Models {
					if m == model {
						found = true
						break
					}
				}
				if !found {
					return current, nil, errors.New("warmup model is not enabled on this channel")
				}
			}
		}
		return current, oauth, nil
	}
	if _, _, err := check(""); err != nil {
		return "", "", err
	}
	c.OAuth = expected
	token, err := svc.bearerCredentialLocked(ctx, c, usageHTTPClient(svc.Client), id)
	if err != nil {
		return "", "", errors.New("could not obtain the selected Codex credential; check account authorization")
	}
	if token == "" {
		return "", "", errors.New("selected Codex credential returned no access token")
	}
	_, oauth, err := check(token)
	if err != nil {
		return "", "", err
	}
	if oauth.AccessToken != token {
		return "", "", errors.New("Codex account changed before action")
	}
	return token, AccountHeaderID(token, oauth.IDToken, oauth.Account, oauth.Subject), nil
}

func (svc *Service) ResetCreditsForCredential(ctx context.Context, c store.Channel, credentialID string) (ResetCredits, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock := svc.lockFor(c.ID)
	if err := lockUsage(ctx, lock); err != nil {
		return ResetCredits{}, err
	}
	defer lock.Unlock()
	token, account, err := svc.actionCredentialLocked(ctx, c, credentialID, false, "")
	if err != nil {
		return ResetCredits{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.Endpoints.resetCreditsURL(), nil)
	if err != nil {
		return ResetCredits{}, errors.New("invalid reset credits endpoint")
	}
	ApplyRequestHeaders(req.Header, token, account, "")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	resp, err := accessguard.Do(usageHTTPClient(svc.Client), req)
	if err != nil {
		if accessguard.Denied(err) {
			return ResetCredits{}, err
		}
		return ResetCredits{}, errors.New("reset credits request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ResetCredits{}, fmt.Errorf("reset credits endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, usageBodyLimit+1))
	if err != nil || len(body) > usageBodyLimit {
		return ResetCredits{}, errors.New("could not read bounded reset credits response")
	}
	out, err := parseResetCredits(body, time.Now().UTC())
	if err != nil {
		return ResetCredits{}, errors.New("reset credits endpoint returned an invalid report")
	}
	for i := range out.Credits {
		for _, dst := range []*string{&out.Credits[i].ID, &out.Credits[i].Type, &out.Credits[i].Status} {
			*dst = firstN(strings.ReplaceAll(*dst, token, "[redacted]"), 120)
		}
	}
	return out, nil
}

func validRedeemRequestID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id[14] != '4' || !strings.ContainsRune("89abAB", rune(id[19])) {
		return false
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func (svc *Service) ConsumeResetCreditForCredential(ctx context.Context, c store.Channel, credentialID, redeemRequestID string) error {
	return svc.consumeResetCredit(ctx, c, credentialID, redeemRequestID, nil)
}

func (svc *Service) consumeResetCredit(ctx context.Context, c store.Channel, credentialID, redeemRequestID string, automatic *store.CredentialOperationsConfig) error {
	if !validRedeemRequestID(redeemRequestID) {
		return actionError("redeem request ID must be a UUIDv4", false)
	}
	return svc.credentialPOST(ctx, c, credentialID, "", svc.Endpoints.resetCreditsURL()+"/consume", map[string]string{"redeem_request_id": redeemRequestID}, false, automatic)
}

func (svc *Service) WarmupForCredential(ctx context.Context, c store.Channel, credentialID, model string) error {
	return svc.warmupCredential(ctx, c, credentialID, model, nil)
}

func (svc *Service) warmupCredential(ctx context.Context, c store.Channel, credentialID, model string, automatic *store.CredentialOperationsConfig) error {
	if strings.TrimSpace(model) == "" {
		return actionError("an enabled warmup model must be selected", false)
	}
	payload := map[string]any{
		"model": model, "instructions": "", "stream": true, "store": false,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "hi"}}}},
	}
	return svc.credentialPOST(ctx, c, credentialID, model, strings.TrimRight(svc.Endpoints.Backend, "/")+"/responses", payload, true, automatic)
}

func (svc *Service) credentialPOST(ctx context.Context, c store.Channel, credentialID, model, endpoint string, payload any, warmup bool, automatic *store.CredentialOperationsConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock := svc.lockFor(c.ID)
	if err := lockUsage(ctx, lock); err != nil {
		return actionError("Codex action cancelled before sending", false)
	}
	defer lock.Unlock()
	token, account, err := svc.actionCredentialLocked(ctx, c, credentialID, true, model)
	if err != nil {
		return actionError(err.Error(), false)
	}
	if automatic != nil {
		live, err := svc.Store.Channel(c.ID)
		if err != nil || scheduleRevision(live, credentialID) != scheduleRevision(c, credentialID) || !sameOperationConfig(*automatic, operationConfig(live, credentialID)) {
			return actionError("自动配置已改变，本次操作已取消", false)
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return actionError("could not encode Codex action", false)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return actionError("invalid Codex action endpoint", false)
	}
	// No GetBody means Go's transport cannot replay this POST as an idempotent
	// request, including when an injected client adds an idempotency header.
	req.GetBody = nil
	ApplyRequestHeaders(req.Header, token, account, "")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if warmup {
		req.Header.Set("Accept", "text/event-stream")
	}
	if ctx.Err() != nil {
		return actionError("Codex action cancelled before sending", false)
	}
	resp, err := accessguard.Do(usageHTTPClient(svc.Client), req)
	if err != nil {
		if accessguard.Denied(err) {
			return actionError("管理授权已撤销或权限已改变，本次动作未发送", false)
		}
		return actionError("Codex action result is unknown; request was not retried", true)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		unknown := resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500
		return actionError(fmt.Sprintf("Codex action returned HTTP %d", resp.StatusCode), unknown)
	}
	if !warmup {
		return nil
	} // Accepted, not proof that quota has reset.
	return readWarmupCompletion(resp.Body)
}

func readWarmupCompletion(body io.Reader) error {
	scanner := bufio.NewScanner(io.LimitReader(body, usageBodyLimit+1))
	scanner.Buffer(make([]byte, 4096), usageBodyLimit+1)
	var event string
	var data []string
	total := 0
	finish := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		raw := strings.Join(data, "\n")
		if raw == "[DONE]" {
			return false, nil
		} // No Responses result status.
		var msg struct {
			Type     string `json:"type"`
			Response *struct {
				Status string `json:"status"`
			} `json:"response"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			return false, actionError("warmup returned an invalid event", true)
		}
		kind := msg.Type
		if kind == "" {
			kind = event
		}
		status := msg.Status
		if msg.Response != nil {
			status = msg.Response.Status
		}
		if kind == "error" || kind == "response.error" || kind == "response.failed" || kind == "response.incomplete" || status == "failed" || status == "incomplete" || status == "cancelled" {
			return false, actionError("warmup returned a failed or incomplete response", false)
		}
		if kind == "response.completed" || kind == "response.done" {
			if status == "completed" {
				return true, nil
			}
			return false, actionError("warmup terminal event did not confirm completion", true)
		}
		return false, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > usageBodyLimit {
			return actionError("warmup response exceeds size limit", true)
		}
		if line == "" {
			done, err := finish()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			event, data = "", nil
		} else if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	// EOF is not an SSE event boundary and cannot establish a terminal result.
	return actionError("warmup ended without a confirmed completion", true)
}
