package statedb

// WriteObservedAcknowledged records only a collector's observed flag.
// matrix/hardened: status, operator watermark and explicit ACK timestamp all
// remain in the WHERE clause, preventing a late probe from undoing user input.
// Producer observations do not touch the operator watermark or ACK timestamp.
func (s *StateDB) WriteObservedAcknowledged(id, expectedStatus string, ack bool, expectedTouch, expectedAckAt int64) (bool, error) {
	value := 0
	if ack {
		value = 1
	}
	var applied bool
	err := withBusyRetry(func() error {
		applied = false
		result, err := s.db.Exec(`UPDATE instances SET acknowledged=? WHERE id=? AND status=?
		AND COALESCE((SELECT CAST(value AS INTEGER) FROM metadata WHERE key='last_modified'),0)=?
		AND CASE WHEN json_valid(tool_data) THEN CAST(COALESCE(json_extract(tool_data,'$.runtime_ack_at'),0) AS INTEGER) ELSE 0 END=?`, value, id, expectedStatus, expectedTouch, expectedAckAt)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		applied = count > 0
		return err
	})
	return applied, err
}
