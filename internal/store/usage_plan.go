package store

import (
	"context"
	"database/sql"
)

// This is a current configuration warning, not a fabricated historical charge.
// Include plan identities actually observed in the selected requests; when no
// request-only dimension is selected, also show declared but unconfigured
// channels even if they have never been called.
func unconfiguredPlanChannelsInTx(ctx context.Context, tx *sql.Tx, f UsageFilter, requested map[int64]bool) (int, error) {
	if f.Model == "" && f.Status == 0 {
		query := `SELECT id FROM channels WHERE EXISTS (SELECT 1 FROM json_each(channels.data,'$.tags') WHERE value='billing:plan')`
		args := []any{}
		if f.ChannelID != 0 {
			query += " AND id=?"
			args = append(args, f.ChannelID)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			requested[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
	}
	if len(requested) == 0 {
		return 0, nil
	}
	// A preserved schedule only covers requests while the channel is declared
	// as a plan and that schedule is active. Leaving plan pauses the schedule;
	// independent plan models still need the declaration restored.
	rows, err := tx.QueryContext(ctx, `SELECT p.channel_id FROM plan_prices p JOIN channels c ON c.id=p.channel_id
 WHERE p.active_from IS NOT NULL
 AND EXISTS (SELECT 1 FROM json_each(c.data,'$.tags') WHERE value='billing:plan')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return 0, err
		}
		delete(requested, id)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	return len(requested), nil
}
