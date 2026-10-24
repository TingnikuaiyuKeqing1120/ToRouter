package store

import (
	"errors"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type APIKeyImport struct {
	Enabled         *bool      `json:"enabled,omitempty"`
	DisplayIcon     string     `json:"display_icon,omitempty"`
	DiscoveryURL    string     `json:"discovery_url,omitempty"`
	ChannelID       int64      `json:"channel_id"`
	ExpectedVersion string     `json:"expected_version"`
	Name            string     `json:"name"`
	Endpoints       []Endpoint `json:"endpoints"`
	Keys            []string   `json:"keys"`
}

// Results contain counts and an unambiguous destination only, never keys.
type APIKeyImportResult struct {
	ChannelVersion string `json:"-"`
	ChannelID      int64  `json:"channel_id"`
	Added          int    `json:"added"`
	Skipped        int    `json:"skipped"`
}

var ErrInvalidAPIKeyImport = errors.New("invalid API key import")

// APIKeyEndpointIdentity compares effective endpoint sets without changing a
// channel or discarding path/query semantics. Empty means invalid. State is an
// exact part of the continuation contract, including unset versus stateless.
func APIKeyEndpointIdentity(c Channel) string {
	endpoints := c.Endpoints
	if len(endpoints) == 0 {
		return ""
	}
	entries := make([]string, 0, len(endpoints))
	seen := map[string]bool{}
	for _, endpoint := range endpoints {
		switch endpoint.Format {
		case FormatOpenAIChatCompletions, FormatOpenAIResponses, FormatAnthropicMessages, FormatGeminiGenerateContent:
		default:
			return ""
		}
		if seen[endpoint.Format] {
			return ""
		}
		seen[endpoint.Format] = true
		if endpoint.State != "" && endpoint.State != "stateless" && endpoint.State != "server_state" {
			return ""
		}
		rawURL := c.EndpointBaseURL(endpoint)
		u, err := url.Parse(rawURL)
		if err != nil || u.User != nil || u.Hostname() == "" || strings.Contains(rawURL, "#") || u.Opaque != "" || strings.HasSuffix(u.Host, ":") {
			return ""
		}
		u.Scheme = strings.ToLower(u.Scheme)
		if u.Scheme != "http" && u.Scheme != "https" {
			return ""
		}
		host, port := strings.ToLower(u.Hostname()), u.Port()
		if port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return ""
			}
			if (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
				port = ""
			}
		}
		if port != "" {
			u.Host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		} else {
			u.Host = host
		}
		entries = append(entries, EntityVersion([]string{endpoint.Format, u.String(), endpoint.State}))
	}
	sort.Strings(entries)
	return EntityVersion(entries)
}

// Existing URL/key pairs remain existing even when a newly observed source
// declares another protocol. Discovery never widens the old channel's formats
// and must not create a duplicate just because its declaration differs.
func apiKeyURLs(c Channel) map[string]bool {
	seen := map[string]bool{}
	for _, ep := range c.Endpoints {
		id := APIKeyEndpointIdentity(Channel{Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: c.EndpointBaseURL(ep)}}})
		if id != "" {
			seen[id] = true
		}
	}
	return seen
}

// ImportAPIKeys commits one destination atomically. Permission rechecks run
// inside the lifecycle lock, before opening saveChannelLocked's transaction.
func (s *Store) ImportAPIKeys(input APIKeyImport, check func() error) (APIKeyImportResult, error) {
	var result APIKeyImportResult
	identity := APIKeyEndpointIdentity(Channel{Endpoints: input.Endpoints})
	if ValidateDiscoveryIcon(input.DisplayIcon) != nil {
		return result, ErrInvalidAPIKeyImport
	}
	if input.ChannelID == 0 && input.ExpectedVersion != "" {
		return result, ErrInvalidAPIKeyImport
	}
	if input.DiscoveryURL != "" {
		for _, ep := range input.Endpoints {
			if DiscoveryURLIdentity(ep.BaseURL) != DiscoveryURLIdentity(input.DiscoveryURL) {
				return result, ErrInvalidAPIKeyImport
			}
		}
	}
	if input.ChannelID < 0 || identity == "" || len(input.Keys) < 1 || len(input.Keys) > 100 {
		return result, ErrInvalidAPIKeyImport
	}
	for _, key := range input.Keys {
		if strings.TrimSpace(key) == "" || len(key) > 16*1024 {
			return result, ErrInvalidAPIKeyImport
		}
	}
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	var destination Channel
	if input.ChannelID > 0 {
		var err error
		destination, err = s.channel(input.ChannelID)
		if err != nil {
			return result, err
		}
		if input.ExpectedVersion == "" || EntityVersion(destination) != input.ExpectedVersion {
			return result, ErrEditConflict
		}
		if destination.CredentialType != CredentialAPIKey || input.DiscoveryURL == "" && APIKeyEndpointIdentity(destination) != identity {
			return result, ErrInvalidAPIKeyImport
		}
		result.ChannelID = destination.ID
		result.ChannelVersion = EntityVersion(destination)
	} else {
		destination = Channel{Name: input.Name, CredentialType: CredentialAPIKey, Enabled: true, Endpoints: append([]Endpoint(nil), input.Endpoints...), Models: []string{}}
		if input.Enabled != nil {
			destination.Enabled = *input.Enabled
		}
	}
	channels, err := s.ListChannels()
	if err != nil {
		return APIKeyImportResult{}, err
	}
	if input.DiscoveryURL != "" {
		if err := ValidateDiscoveryTarget(channels, input.DiscoveryURL, input.ChannelID); err != nil {
			return result, err
		}
	}
	owners := map[string]map[int64]bool{}
	urls := apiKeyURLs(Channel{Endpoints: input.Endpoints})
	for _, channel := range channels {
		if channel.CredentialType != CredentialAPIKey {
			continue
		}
		overlap := false
		for address := range apiKeyURLs(channel) {
			if urls[address] {
				overlap = true
				break
			}
		}
		if !overlap {
			continue
		}
		for _, credential := range channel.Credentials {
			if credential.APIKey != "" {
				if owners[credential.APIKey] == nil {
					owners[credential.APIKey] = map[int64]bool{}
				}
				owners[credential.APIKey][channel.ID] = true
			}
		}
	}
	seen := map[string]bool{}
	matched := map[int64]bool{}
	for _, key := range input.Keys {
		for owner := range owners[key] {
			matched[owner] = true
		}
		if seen[key] || len(owners[key]) > 0 {
			result.Skipped++
			continue
		}
		seen[key] = true
		id, err := randomToken("cred_")
		if err != nil {
			return APIKeyImportResult{}, err
		}
		destination.Credentials = append(destination.Credentials, CredentialItem{ID: id, Label: "导入凭据", APIKey: key, Enabled: true})
		result.Added++
	}
	if check != nil {
		if err := check(); err != nil {
			return APIKeyImportResult{}, err
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
	if err := s.saveChannelLocked(&destination, false); err != nil {
		return APIKeyImportResult{}, err
	}
	result.ChannelID = destination.ID
	result.ChannelVersion = EntityVersion(destination)
	if err := s.SaveDiscoveryIcon(destination.ID, input.DisplayIcon); err != nil {
		return result, err
	}
	return result, nil
}
