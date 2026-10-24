package store

func ManagementConnectionVersion(v ManagementConnection) string {
	v.LastUsedAt = nil
	return EntityVersion(v)
}

func (s *Store) RevokeManagementConnection(id int64, expected string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := scanManagement(tx.QueryRow(managementSelect+" WHERE id=?", id))
	if err != nil {
		return err
	}
	if expected == "" || ManagementConnectionVersion(v) != expected {
		return ErrEditConflict
	}
	if _, err = tx.Exec("DELETE FROM management_connections WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
