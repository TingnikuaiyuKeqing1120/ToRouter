package claude

import (
	"context"
	"errors"
	"time"
	"torouter/internal/store"
)

// CompleteImport runs only after confirmation, using the existing refresh and
// profile paths to obtain actual request identity. It never invents an account.
func (d *Driver) CompleteImport(ctx context.Context, c store.OAuthCredential) (store.OAuthCredential, error) {
	var err error
	if c.ClientID == "" {
		c.ClientID = ClientID
	}
	if c.DeviceID == "" {
		c.DeviceID, err = newDeviceID()
		if err != nil {
			return c, err
		}
	}
	if c.AccessToken == "" || !c.ExpiresAt.IsZero() && time.Until(c.ExpiresAt) <= 0 {
		next, refreshErr := d.Refresh(ctx, c)
		if refreshErr != nil {
			return c, refreshErr
		}
		c = next
	}
	if c.Account == "" {
		d.inspect(ctx, &c)
	}
	if c.Account == "" {
		return c, errors.New("Claude import profile omitted account identity")
	}
	if c.Subject == "" {
		c.Subject = c.Account
	}
	return c, nil
}
