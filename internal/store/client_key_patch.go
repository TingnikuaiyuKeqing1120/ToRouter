package store

func ClientKeyVersion(key ClientKey) string { key.LastUsedAt = nil; return EntityVersion(key) }

func (s *Store) PatchClientKey(id int64, expected string, name *string, scope *ClientKeyScope) (ClientKey, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	rows, err := s.ListClientKeys()
	if err != nil {
		return ClientKey{}, err
	}
	for _, key := range rows {
		if key.ID == id {
			if expected == "" || ClientKeyVersion(key) != expected {
				return ClientKey{}, ErrEditConflict
			}
			if name != nil {
				key.Name = *name
			}
			if scope != nil {
				key.Scope = *scope
			}
			return s.updateClientKeyLocked(id, key.Name, key.Scope)
		}
	}
	return ClientKey{}, ErrNotFound
}
