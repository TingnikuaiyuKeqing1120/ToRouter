package store

// DiscoveryURLIdentity preserves the complete URL path while ignoring protocol
// declarations and continuation state, which do not identify a URL card.
func DiscoveryURLIdentity(address string) string {
	return APIKeyEndpointIdentity(Channel{Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: address}}})
}

func DiscoveryTargets(channels []Channel, address string) []Channel {
	identity := DiscoveryURLIdentity(address)
	out := []Channel{}
	if identity == "" {
		return out
	}
	for _, channel := range channels {
		for _, endpoint := range channel.Endpoints {
			if DiscoveryURLIdentity(channel.EndpointBaseURL(endpoint)) == identity {
				out = append(out, channel)
				break
			}
		}
	}
	return out
}

// ValidateDiscoveryTarget is called under channelSaveMu. Creation is independent
// of existing URL owners; append must explicitly name an owner of this URL.
func ValidateDiscoveryTarget(channels []Channel, address string, id int64) error {
	if DiscoveryURLIdentity(address) == "" {
		return ErrEditConflict
	}
	targets := DiscoveryTargets(channels, address)
	if id == 0 {
		return nil
	}
	for _, target := range targets {
		if target.ID == id {
			return nil
		}
	}
	return ErrEditConflict
}

func ValidateDiscoveryIcon(icon string) error { return validateChannelIcon(icon) }

// Discovery icons reuse console display preferences rather than routing data.
func (s *Store) SaveDiscoveryIcon(channelID int64, icon string) error {
	if icon == "" {
		return nil
	}
	_, err := s.PatchChannelDisplay(ChannelDisplayPatch{Icon: &ChannelDisplayIcon{ChannelID: channelID, Value: icon}})
	return err
}
