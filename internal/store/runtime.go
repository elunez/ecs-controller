package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
)

func (s *Store) RuntimeStatus(key string) (app.RuntimeStatus, error) {
	var item app.RuntimeStatus
	err := s.DB.QueryRow(`SELECT component_key,label,status,last_started_at,last_success_at,last_failure_at,last_duration_ms,next_run_at,last_error,detail,updated_at FROM runtime_status WHERE component_key=?`, key).
		Scan(&item.Key, &item.Label, &item.Status, &item.LastStartedAt, &item.LastSuccessAt, &item.LastFailureAt, &item.LastDurationMS, &item.NextRunAt, &item.LastError, &item.Detail, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		item.Key = key
		return item, nil
	}
	return item, err
}

func (s *Store) RuntimeStatuses() ([]app.RuntimeStatus, error) {
	rows, err := s.DB.Query(`SELECT component_key,label,status,last_started_at,last_success_at,last_failure_at,last_duration_ms,next_run_at,last_error,detail,updated_at FROM runtime_status ORDER BY sort_order,component_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]app.RuntimeStatus, 0)
	for rows.Next() {
		var item app.RuntimeStatus
		if err := rows.Scan(&item.Key, &item.Label, &item.Status, &item.LastStartedAt, &item.LastSuccessAt, &item.LastFailureAt, &item.LastDurationMS, &item.NextRunAt, &item.LastError, &item.Detail, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) SetRuntimeStatus(item app.RuntimeStatus, sortOrder int) error {
	item.UpdatedAt = time.Now().Unix()
	_, err := s.DB.Exec(`INSERT INTO runtime_status(component_key,label,status,last_started_at,last_success_at,last_failure_at,last_duration_ms,next_run_at,last_error,detail,updated_at,sort_order)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(component_key) DO UPDATE SET label=excluded.label,status=excluded.status,last_started_at=excluded.last_started_at,last_success_at=excluded.last_success_at,last_failure_at=excluded.last_failure_at,last_duration_ms=excluded.last_duration_ms,next_run_at=excluded.next_run_at,last_error=excluded.last_error,detail=excluded.detail,updated_at=excluded.updated_at,sort_order=excluded.sort_order`,
		item.Key, item.Label, item.Status, item.LastStartedAt, item.LastSuccessAt, item.LastFailureAt, item.LastDurationMS, item.NextRunAt, item.LastError, item.Detail, item.UpdatedAt, sortOrder)
	return err
}

func (s *Store) JobStatusCounts() (map[string]int, error) {
	rows, err := s.DB.Query(`SELECT status,COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{"queued": 0, "running": 0, "retry": 0, "failed": 0}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

func (s *Store) FailedJobs(limit int) ([]map[string]any, error) {
	if limit < 1 || limit > 20 {
		limit = 5
	}
	rows, err := s.DB.Query(`SELECT job_id,kind,entity_key,attempts,last_error,updated_at FROM jobs WHERE status='failed' ORDER BY updated_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var jobID, kind, entityKey, lastError string
		var attempts int
		var updatedAt int64
		if err := rows.Scan(&jobID, &kind, &entityKey, &attempts, &lastError, &updatedAt); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"job_id": jobID, "kind": kind, "entity_key": entityKey, "attempts": attempts, "last_error": sanitizeRuntimeJobError(lastError), "updated_at": updatedAt})
	}
	return items, rows.Err()
}

func (s *Store) RequeueFailedJob(jobID string) (bool, error) {
	now := time.Now().Unix()
	result, err := s.DB.Exec(`UPDATE jobs SET status='retry',attempts=0,locked_at=0,available_at=?,last_error='',updated_at=? WHERE job_id=? AND status='failed'`, now, now, jobID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func sanitizeRuntimeJobError(message string) string {
	if len(message) > 500 {
		return message[:500] + "…"
	}
	return message
}
