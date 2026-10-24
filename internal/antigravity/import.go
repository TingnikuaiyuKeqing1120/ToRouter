package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"torouter/internal/store"
)

// CompleteImport enriches confirmed foreign material with the same Google
// identity/project control calls used by authorization, without repeating login.
func (d *Driver) CompleteImport(ctx context.Context, c store.OAuthCredential) (store.OAuthCredential, error) {
	if c.ClientID == "" {
		c.ClientID = d.endpoints.ClientID
	}
	if c.AccessToken == "" || !c.ExpiresAt.IsZero() && time.Until(c.ExpiresAt) <= 0 {
		next, refreshErr := d.Refresh(ctx, c)
		if refreshErr != nil {
			return c, refreshErr
		}
		c = next
	}
	if c.Subject == "" || c.ProjectID == "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.endpoints.UserInfoURL, nil)
		if err != nil {
			return c, err
		}
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
		b, err := d.control(req, "userinfo", c, c.AccessToken, c.RefreshToken)
		if err != nil {
			return c, err
		}
		var who struct {
			Email string `json:"email"`
			ID    string `json:"id"`
		}
		if json.Unmarshal(b, &who) != nil || who.ID == "" || who.Email == "" {
			return c, errors.New("antigravity: import profile omitted account identity")
		}
		c.Subject, c.Account = who.ID, who.ID
		if c.ProjectID == "" {
			c.ProjectID, err = d.project(ctx, c, who.Email)
			if err != nil {
				return c, err
			}
		}
	}
	return c, nil
}
