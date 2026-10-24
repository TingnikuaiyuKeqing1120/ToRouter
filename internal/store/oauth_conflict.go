package store

// OAuthOwnerConflict reports location without exposing identity or tokens.
type OAuthOwnerConflict struct {
	ChannelID    int64
	CredentialID string
}

func (e *OAuthOwnerConflict) Error() string {
	return "账号已存在，请在原凭证重新授权或显式移动"
}
func (e *OAuthOwnerConflict) Unwrap() error { return ErrNativeOAuthIdentity }
