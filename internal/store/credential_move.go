package store

import (
	"encoding/json"
	"fmt"
	"sync"
)

// CredentialLifecycleMutex is shared by refresh, consent and membership moves.
// Acquire it before channelSaveMu; two-channel operations acquire IDs in order.
func (s *Store) CredentialLifecycleMutex(id int64) *sync.Mutex {
	v, _ := s.credentialLifecycle.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Store) MoveCredentials(sourceID, targetID int64, ids []string, expected, expectedTarget int64, check func() error) (Channel, Channel, error) {
	if sourceID < 1 || targetID < 1 || sourceID == targetID || len(ids) == 0 {
		return Channel{}, Channel{}, fmt.Errorf("%w: 请选择不同渠道和需要移动的凭证", ErrInvalidCredentialOperation)
	}
	first, second := sourceID, targetID
	if first > second {
		first, second = second, first
	}
	a, b := s.CredentialLifecycleMutex(first), s.CredentialLifecycleMutex(second)
	a.Lock()
	defer a.Unlock()
	b.Lock()
	defer b.Unlock()
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	source, err := s.channel(sourceID)
	if err != nil {
		return Channel{}, Channel{}, err
	}
	target, err := s.channel(targetID)
	if err != nil {
		return Channel{}, Channel{}, err
	}
	if expected == 0 || expectedTarget == 0 || source.Generation != expected || target.Generation != expectedTarget {
		return Channel{}, Channel{}, ErrEditConflict
	}
	_, builtinErr := BuiltinChannel("validate", source.CredentialType)
	if builtinErr != nil || source.CredentialType == CredentialZenFree || source.CredentialType != target.CredentialType {
		return Channel{}, Channel{}, fmt.Errorf("%w: 仅支持相同对象及地区的账号渠道移动", ErrInvalidCredentialOperation)
	}
	selected := map[string]bool{}
	for _, id := range ids {
		if id == "" || selected[id] {
			return Channel{}, Channel{}, ErrEditConflict
		}
		selected[id] = true
	}
	existing := map[string]bool{}
	for _, item := range target.Credentials {
		existing[item.ID] = true
	}
	remaining := make([]CredentialItem, 0, len(source.Credentials))
	moved := []CredentialItem{}
	for _, item := range source.Credentials {
		if selected[item.ID] {
			if existing[item.ID] {
				return Channel{}, Channel{}, fmt.Errorf("%w: 目标渠道已有相同凭证ID", ErrInvalidCredentialOperation)
			}
			moved = append(moved, item)
			delete(selected, item.ID)
		} else {
			remaining = append(remaining, item)
		}
	}
	if len(selected) > 0 {
		return Channel{}, Channel{}, ErrEditConflict
	}
	source.Credentials = remaining
	target.Credentials = append(target.Credentials, moved...)
	source.APIKey, source.OAuth = "", nil
	target.APIKey, target.OAuth = "", nil
	source.normalize()
	target.normalize()
	source.Generation++
	target.Generation++
	if check != nil {
		if err = check(); err != nil {
			return Channel{}, Channel{}, err
		}
	}
	// Seal before opening the transaction: neither endpoint can be partially written.
	raws := make([][]byte, 2)
	for i, c := range []Channel{source, target} {
		row, e := s.sealChannel(c)
		if e != nil {
			return Channel{}, Channel{}, e
		}
		raws[i], e = json.Marshal(row)
		if e != nil {
			return Channel{}, Channel{}, e
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Channel{}, Channel{}, err
	}
	defer tx.Rollback()
	for i, id := range []int64{sourceID, targetID} {
		if _, err = tx.Exec("UPDATE channels SET data=? WHERE id=?", string(raws[i]), id); err != nil {
			return Channel{}, Channel{}, err
		}
	}
	for _, item := range moved {
		if source.CredentialType == CredentialCodexOAuth {
			if _, err = tx.Exec("UPDATE meta SET key=? WHERE key=?", credentialOperationKey(targetID, item.ID), credentialOperationKey(sourceID, item.ID)); err != nil {
				return Channel{}, Channel{}, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return Channel{}, Channel{}, err
	}
	return source, target, nil
}
