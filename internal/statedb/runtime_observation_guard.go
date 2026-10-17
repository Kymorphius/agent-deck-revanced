package statedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// matrix/hardened: a user edit fences all writes from a probe that started
// before that edit. Producer native/activity writes deliberately do not Touch.
var ErrObservationStale = errors.New("runtime observation superseded by a registry mutation")

// GuardRuntimeObservation pins the SQLite writer generation while applying an
// observation sidecar mutation. The callback must not call back into this DB.
// matrix/hardened: a late healthy/auth probe cannot remove or recreate the
// sidecar of a replacement launch after its registry mutation commits.
func (s *StateDB) GuardRuntimeObservation(id string, expectedTouch int64, action func() error) error {
	return withBusyRetry(func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.Exec(`UPDATE instances SET status=status WHERE id=? AND status<>'stopped' AND COALESCE((SELECT CAST(value AS INTEGER) FROM metadata WHERE key='last_modified'),0)=?`, id, expectedTouch)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrObservationStale
		}
		if err := action(); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func guardRuntimeObservation(tx *sql.Tx, id string, expectedTouch []int64) error {
	if len(expectedTouch) == 0 {
		return nil
	}
	var current int64
	var status string
	err := tx.QueryRow(`SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM metadata WHERE key='last_modified'), 0), status FROM instances WHERE id=?`, id).Scan(&current, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrObservationStale
	}
	if err != nil {
		return err
	}
	if current != expectedTouch[0] || status == "stopped" {
		return ErrObservationStale
	}
	return nil
}

// WriteObservedStatusIfCurrent atomically claims a verdict only while both
// its previous status and the persisted configuration generation still match.
func (s *StateDB) WriteObservedStatusIfCurrent(id, expected, status, tool string, expectedTouch int64) (bool, error) {
	observe := loadStatusChangeObserver()
	var applied bool
	var tmuxName string
	err := withBusyRetry(func() error {
		applied = false
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if observe != nil {
			_ = tx.QueryRow(`SELECT tmux_session FROM instances WHERE id=?`, id).Scan(&tmuxName)
		}
		res, err := tx.Exec(writeStatusSQL+` AND status=? AND COALESCE((SELECT CAST(value AS INTEGER) FROM metadata WHERE key='last_modified'),0)=?`, status, tool, status, id, expected, expectedTouch)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		applied = n > 0
		return tx.Commit()
	})
	if err == nil && applied && expected != status && observe != nil {
		observe(StatusChange{DBPath: s.path, ID: id, TmuxSession: tmuxName, From: expected, To: status, Tool: tool, At: time.Now()})
	}
	return applied, err
}

// guardedRuntimeUpdate wraps small producer-only updates in the same SQLite
// transaction as their registry generation check; caller errors roll back.
func (s *StateDB) guardedRuntimeUpdate(id string, expected []int64, query string, args ...any) error {
	return withBusyRetry(func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := guardRuntimeObservation(tx, id, expected); err != nil {
			return err
		}
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("runtime update: %w", err)
		}
		return tx.Commit()
	})
}
