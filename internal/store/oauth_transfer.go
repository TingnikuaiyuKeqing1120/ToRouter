package store

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// Transfer items are private migration material, not console/MCP DTOs. The
// caller may release them only through an explicit sensitive export and must
// never return them as a preview. No source channel configuration or automatic actions travel here.
type OAuthTransferRef struct {
	ChannelID    int64  `json:"channel_id"`
	CredentialID string `json:"credential_id"`
}
type OAuthTransferItem struct {
	CredentialType string           `json:"credential_type"`
	Label          string           `json:"label"`
	Enabled        bool             `json:"enabled"`
	Endpoints      []Endpoint       `json:"endpoints"`
	OAuth          *OAuthCredential `json:"oauth"`
}
type OAuthTransferImport struct {
	ChannelID       int64               `json:"channel_id"`
	ExpectedVersion string              `json:"expected_version"`
	Name            string              `json:"name"`
	Enabled         *bool               `json:"enabled,omitempty"`
	DisplayIcon     string              `json:"display_icon,omitempty"`
	DiscoveryURL    string              `json:"discovery_url,omitempty"`
	Items           []OAuthTransferItem `json:"items"`
}

// Results deliberately exclude all account identifiers, labels and materials.
type OAuthTransferImportResult struct {
	ChannelVersion string `json:"-"`
	ChannelID      int64  `json:"channel_id"`
	Added          int    `json:"added"`
	Skipped        int    `json:"skipped"`
}

var ErrInvalidOAuthTransfer = errors.New("OAuth transfer material or destination is invalid")
var ErrOAuthTransferFailed = errors.New("OAuth transfer storage operation failed")
var ErrOAuthTransferDenied = errors.New("OAuth transfer authorization changed")

// Preserve the caller's denial identity without printing its possibly private
// message. Database/validation failures below expose fixed safe errors only.
type oauthTransferDenied struct{ cause error }

func (e oauthTransferDenied) Error() string        { return ErrOAuthTransferDenied.Error() }
func (e oauthTransferDenied) Unwrap() error        { return e.cause }
func (e oauthTransferDenied) Is(target error) bool { return target == ErrOAuthTransferDenied }

func oauthTransferType(kind string) bool {
	return kind == CredentialCodexOAuth || kind == CredentialChatGPTOAuth || kind == CredentialCopilotOAuth || IsNativeOAuth(kind) || IsCompatibleOAuth(kind)
}
func copyOAuthTransferItem(item OAuthTransferItem) OAuthTransferItem {
	item.Endpoints = append([]Endpoint(nil), item.Endpoints...)
	if item.OAuth != nil {
		material := *item.OAuth
		item.OAuth = &material
	}
	return item
}

// Validate every member independently. ValidateChannel alone on a pool would
// inspect only the primary credential for the three older OAuth tracks.
func ValidateOAuthTransferItem(item OAuthTransferItem) error {
	if !oauthTransferType(item.CredentialType) || item.OAuth == nil || APIKeyEndpointIdentity(Channel{Endpoints: item.Endpoints}) == "" {
		return ErrInvalidOAuthTransfer
	}
	o := item.OAuth
	if item.CredentialType == CredentialChatGPTOAuth && strings.TrimSpace(o.Subject) == "" {
		return ErrInvalidOAuthTransfer
	}
	// Compatible credentials validate their complete, fixed profile API below;
	// the older host-based helper cannot compare APIs that include a path.
	if !IsCompatibleOAuth(item.CredentialType) && o.APIBase != "" && (!validBaseURL(o.APIBase) || !OAuthHostAllowed(item.CredentialType, o.APIBase) || !strings.HasPrefix(strings.ToLower(o.APIBase), "https://") && !EndpointRelaxedForDiagnostics(o.APIBase)) {
		return ErrInvalidOAuthTransfer
	}
	single := Channel{Name: "OAuth transfer", CredentialType: item.CredentialType, Enabled: item.Enabled, Endpoints: append([]Endpoint(nil), item.Endpoints...), Credentials: []CredentialItem{{ID: "transfer-member", Enabled: item.Enabled, OAuth: o}}}
	if ValidateChannel(single) != nil {
		return ErrInvalidOAuthTransfer
	}
	if oauthTransferPrimaryIdentity(item) == "" {
		return ErrInvalidOAuthTransfer
	}
	return nil
}

// Registration ClientID/HostID, requested/granted Scopes, and API path are not
// the account's identity. Keep them intact as material. Source authority is a
// separate domain so identical subjects from different providers cannot merge.
func oauthTransferDomain(item OAuthTransferItem) string {
	if item.OAuth == nil {
		return ""
	}
	raw := item.OAuth.APIBase
	if raw == "" && len(item.Endpoints) > 0 {
		raw = item.Endpoints[0].BaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if scheme != "https" && scheme != "http" {
		return ""
	}
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}
func oauthTransferPrimaryIdentity(item OAuthTransferItem) string {
	o := item.OAuth
	if o == nil || !oauthTransferType(item.CredentialType) {
		return ""
	}
	if IsCompatibleOAuth(item.CredentialType) {
		// WorkBuddy's Account already includes its enterprise dimension; regions
		// have different fixed provider types/API domains. Do not flatten either.
		return CompatibleOAuthIdentity(item.CredentialType, o)
	}
	domain := oauthTransferDomain(item)
	if domain == "" {
		return ""
	}
	prefix := []string{item.CredentialType, o.Provider, domain}
	switch item.CredentialType {
	case CredentialCopilotOAuth:
		if o.Provider != OAuthProviderCopilot || strings.TrimSpace(o.GitHubToken) == "" {
			return ""
		}
		// Different GitHub tokens are not proof of different or identical accounts.
		return EntityVersion([]string{item.CredentialType, o.Provider, "github_token", o.GitHubToken})
	case CredentialChatGPTOAuth:
		if o.Provider != OAuthProviderChatGPT || strings.TrimSpace(o.Subject) == "" {
			return ""
		}
		return EntityVersion(append(prefix, "subject", o.Subject))
	case CredentialCodexOAuth:
		if o.Provider != OAuthProviderCodex {
			return ""
		}
		sub := strings.TrimSpace(o.Subject)
		acct := strings.TrimSpace(o.Account)
		if sub != "" && acct != "" {
			return EntityVersion(append(prefix, "user_account", sub, acct))
		}
		return EntityVersion(append(prefix, "material", o.AccessToken, o.RefreshToken, o.IDToken))
	default:
		if IsNativeOAuth(item.CredentialType) {
			identity := NativeOAuthAccountIdentity(item.CredentialType, *o)
			if identity != "" {
				return EntityVersion(append(prefix, identity))
			}
		}
	}
	return ""
}
func OAuthTransferIdentity(item OAuthTransferItem) string {
	if ValidateOAuthTransferItem(item) != nil {
		return ""
	}
	return oauthTransferPrimaryIdentity(item)
}

// OAuthTransferIdentityAliases is private correlation evidence for migration
// services. It must not be projected into public preview or receipt DTOs.
func OAuthTransferIdentityAliases(item OAuthTransferItem) []string {
	if ValidateOAuthTransferItem(item) != nil {
		return nil
	}
	return oauthTransferAliases(item)
}

func oauthTransferAliases(item OAuthTransferItem) []string {
	primary := oauthTransferPrimaryIdentity(item)
	if primary == "" {
		return nil
	}
	aliases := []string{primary}
	o := item.OAuth
	domain := oauthTransferDomain(item)
	if item.CredentialType == CredentialCodexOAuth && (o.AccessToken != "" || o.RefreshToken != "") {
		// Link exact complete material so an anonymous copy with identical material matches its stable twin
		materialAlias := EntityVersion(append([]string{item.CredentialType, o.Provider, domain}, "material", o.AccessToken, o.RefreshToken, o.IDToken))
		if materialAlias != primary {
			aliases = append(aliases, materialAlias)
		}
	}
	if item.CredentialType == CredentialDevinOAuth && o.AccessToken != "" {
		aliases = append(aliases, EntityVersion([]string{item.CredentialType, o.Provider, domain, "session", o.AccessToken}))
	}
	return aliases
}

// Export selects exact pool members under the write lifecycle lock. It does
// not discover files, use defaults, contact issuers, refresh, or disable source
// credentials. A transferred source copy is not safe to keep rotating in parallel.
func (s *Store) ExportOAuthCredentials(refs []OAuthTransferRef) ([]OAuthTransferItem, error) {
	if len(refs) < 1 || len(refs) > 100 {
		return nil, ErrInvalidOAuthTransfer
	}
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	seen := map[OAuthTransferRef]bool{}
	out := make([]OAuthTransferItem, 0, len(refs))
	for _, ref := range refs {
		if ref.ChannelID <= 0 || strings.TrimSpace(ref.CredentialID) == "" || seen[ref] {
			return nil, ErrInvalidOAuthTransfer
		}
		seen[ref] = true
		channel, err := s.channel(ref.ChannelID)
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, ErrOAuthTransferFailed
		}
		index := credentialIndex(channel, ref.CredentialID)
		if index < 0 {
			return nil, ErrNotFound
		}
		member := channel.Credentials[index]
		if member.APIKey != "" {
			return nil, ErrInvalidOAuthTransfer
		}
		item := copyOAuthTransferItem(OAuthTransferItem{CredentialType: channel.CredentialType, Label: member.Label, Enabled: member.Enabled, Endpoints: channel.Endpoints, OAuth: member.OAuth})
		for i := range item.Endpoints {
			item.Endpoints[i].BaseURL = channel.EndpointBaseURL(item.Endpoints[i])
		}
		if ValidateOAuthTransferItem(item) != nil {
			return nil, ErrInvalidOAuthTransfer
		}
		out = append(out, item)
	}
	return out, nil
}

// One call appends one homogeneous target in one save. Existing account owners
// are only skipped: this is deliberately not an authorization/rotation commit.
func (s *Store) ImportOAuthCredentials(input OAuthTransferImport, guard func() error) (OAuthTransferImportResult, error) {
	var result OAuthTransferImportResult
	if input.ChannelID < 0 || len(input.Items) < 1 || len(input.Items) > 100 || input.ChannelID == 0 && input.ExpectedVersion != "" {
		return result, ErrInvalidOAuthTransfer
	}
	items := make([]OAuthTransferItem, len(input.Items))
	for i, item := range input.Items {
		items[i] = copyOAuthTransferItem(item)
		if ValidateOAuthTransferItem(items[i]) != nil {
			return result, ErrInvalidOAuthTransfer
		}
	}
	kind := items[0].CredentialType
	endpoints := APIKeyEndpointIdentity(Channel{Endpoints: items[0].Endpoints})
	for _, item := range items {
		if item.CredentialType != kind || APIKeyEndpointIdentity(Channel{Endpoints: item.Endpoints}) != endpoints {
			return result, ErrInvalidOAuthTransfer
		}
	}
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	var destination Channel
	if input.ChannelID > 0 {
		var err error
		destination, err = s.channel(input.ChannelID)
		if errors.Is(err, ErrNotFound) {
			return result, ErrNotFound
		}
		if err != nil {
			return result, ErrOAuthTransferFailed
		}
		if input.ExpectedVersion == "" || EntityVersion(destination) != input.ExpectedVersion {
			return result, ErrEditConflict
		}
		if destination.CredentialType != kind || input.DiscoveryURL == "" && APIKeyEndpointIdentity(destination) != endpoints {
			return result, ErrInvalidOAuthTransfer
		}
		result.ChannelID = destination.ID
		result.ChannelVersion = EntityVersion(destination)
	} else {
		name := input.Name
		if strings.TrimSpace(name) == "" {
			name = "迁入的 OAuth"
		}
		destination = Channel{Name: name, CredentialType: kind, Endpoints: append([]Endpoint(nil), items[0].Endpoints...), Models: []string{}, AvailableModels: []string{}}
	}
	channels, err := s.ListChannels()
	if err != nil {
		return OAuthTransferImportResult{}, ErrOAuthTransferFailed
	}
	if input.DiscoveryURL != "" {
		if err := ValidateDiscoveryTarget(channels, input.DiscoveryURL, input.ChannelID); err != nil {
			return result, err
		}
		for _, item := range items {
			matched := false
			for _, ep := range item.Endpoints {
				if DiscoveryURLIdentity(ep.BaseURL) == DiscoveryURLIdentity(input.DiscoveryURL) {
					matched = true
				}
			}
			if !matched {
				return result, ErrInvalidOAuthTransfer
			}
		}
	}
	if ValidateDiscoveryIcon(input.DisplayIcon) != nil {
		return result, ErrInvalidOAuthTransfer
	}
	// Connect the complete batch and existing pool before deciding ownership.
	// An anonymous token copy can precede the stable-account copy that links it
	// to an existing owner; streaming decisions would depend on input order.
	parents := map[string]string{}
	var root func(string) string
	root = func(identity string) string {
		parent, ok := parents[identity]
		if !ok {
			parents[identity] = identity
			return identity
		}
		if parent != identity {
			parents[identity] = root(parent)
		}
		return parents[identity]
	}
	connect := func(aliases []string) {
		for _, identity := range aliases {
			parents[root(identity)] = root(aliases[0])
		}
	}
	type ownerEvidence struct {
		aliases []string
		id      int64
	}
	var existing []ownerEvidence
	for _, channel := range channels {
		effective := append([]Endpoint(nil), channel.Endpoints...)
		for i := range effective {
			effective[i].BaseURL = channel.EndpointBaseURL(effective[i])
		}
		for _, member := range channel.Credentials {
			item := OAuthTransferItem{CredentialType: channel.CredentialType, Endpoints: effective, OAuth: member.OAuth}
			aliases := OAuthTransferMaterialAliases(item)
			connect(aliases)
			existing = append(existing, ownerEvidence{aliases: aliases, id: channel.ID})
		}
	}
	batchAliases := make([][]string, len(items))
	for i, item := range items {
		batchAliases[i] = OAuthTransferMaterialAliases(item)
		connect(batchAliases[i])
	}
	owners := map[string]map[int64]bool{}
	for _, evidence := range existing {
		if len(evidence.aliases) == 0 {
			continue
		}
		identity := root(evidence.aliases[0])
		if owners[identity] == nil {
			owners[identity] = map[int64]bool{}
		}
		owners[identity][evidence.id] = true
	}
	representatives := map[string]int{}
	for i, aliases := range batchAliases {
		identity := root(aliases[0])
		previous, ok := representatives[identity]
		if !ok {
			representatives[identity] = i
			continue
		}
		old, current := items[previous].OAuth, items[i].OAuth
		if old.Account == "" && old.Subject == "" && (current.Account != "" || current.Subject != "") {
			// Do not discard available stable ownership evidence merely because
			// an otherwise identical token-only copy appeared first.
			representatives[identity] = i
		}
	}
	seen := map[string]bool{}
	matched := map[int64]bool{}
	for i := range items {
		identity := root(batchAliases[i][0])
		duplicate := seen[identity] || len(owners[identity]) > 0
		for id := range owners[identity] {
			matched[id] = true
		}
		seen[identity] = true
		if duplicate {
			result.Skipped++
			continue
		}
		item := items[representatives[identity]]
		id, err := randomToken("cred_")
		if err != nil {
			return OAuthTransferImportResult{}, ErrOAuthTransferFailed
		}
		label := item.Label
		if strings.TrimSpace(label) == "" {
			label = "迁入凭据"
		}
		destination.Credentials = append(destination.Credentials, CredentialItem{ID: id, Label: label, Enabled: item.Enabled, OAuth: item.OAuth})
		if input.ChannelID == 0 && item.Enabled {
			destination.Enabled = true
		}
		result.Added++
	}
	if guard != nil {
		if err := guard(); err != nil {
			return OAuthTransferImportResult{}, oauthTransferDenied{cause: err}
		}
	}
	if result.Added == 0 {
		if result.ChannelID == 0 && len(matched) == 1 {
			for id := range matched {
				result.ChannelID = id
			}
		}
		return result, nil
	}
	if input.ChannelID == 0 && input.Enabled != nil {
		destination.Enabled = *input.Enabled
	}
	if err := s.saveChannelLocked(&destination, false); err != nil {
		return OAuthTransferImportResult{}, ErrOAuthTransferFailed
	}
	result.ChannelID = destination.ID
	result.ChannelVersion = EntityVersion(destination)
	if input.DisplayIcon != "" {
		if err := s.SaveDiscoveryIcon(destination.ID, input.DisplayIcon); err != nil {
			return result, ErrOAuthTransferFailed
		}
	}
	return result, nil
}
