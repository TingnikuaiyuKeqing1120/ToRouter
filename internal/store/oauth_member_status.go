package store

import "time"

func (s *Store) RejectAccessOnlyCredential(expected Channel, credentialID, rejectedToken string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if expected.CredentialType != CredentialCodexOAuth || credentialID == "" || rejectedToken == "" {
		return ErrEditConflict
	}
	c, err := s.channel(expected.ID)
	if err != nil {
		return err
	}
	i, before := credentialIndex(c, credentialID), credentialIndex(expected, credentialID)
	if c.CredentialType != CredentialCodexOAuth || c.Generation != expected.Generation || i < 0 || before < 0 {
		return ErrEditConflict
	}
	live, prior := c.Credentials[i].OAuth, expected.Credentials[before].OAuth
	if live == nil || prior == nil || live.RefreshToken != "" || prior.RefreshToken != "" || live.AccessToken != rejectedToken || prior.AccessToken != rejectedToken || AccountOAuthIdentity(c.CredentialType, live) != AccountOAuthIdentity(c.CredentialType, prior) {
		return ErrEditConflict
	}
	live.Status, live.StatusReason, live.UpdatedAt = OAuthStatusReauthRequired, "上游已拒绝访问令牌且无刷新凭证，请重新授权", time.Now().UTC()
	c.OAuth = nil
	c.normalize()
	return s.persistRow(&c)
}

func (s *Store) SetOAuthCredentialStatus(id int64, credentialID, status, reason string, disable bool) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	i := credentialIndex(c, credentialID)
	if i < 0 || c.Credentials[i].OAuth == nil {
		return ErrNotFound
	}
	o := c.Credentials[i].OAuth
	o.Status, o.StatusReason, o.UpdatedAt = status, reason, time.Now().UTC()
	if disable {
		c.Credentials[i].Enabled = false
	}
	c.OAuth = nil
	c.normalize()
	return s.persistRow(&c)
}
