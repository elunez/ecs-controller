package store

import (
	"regexp"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
)

var auditSensitivePattern = regexp.MustCompile(`(?i)(access[_ -]?key[_ -]?secret|secret(?:id|key)?|password|passwd|token|authorization|cookie)\s*[:=]\s*[^\s,;]+`)

func sanitizeAuditText(value string) string {
	value = strings.TrimSpace(value)
	value = auditSensitivePattern.ReplaceAllStringFunc(value, func(match string) string {
		if index := strings.IndexAny(match, ":="); index >= 0 {
			return strings.TrimSpace(match[:index]) + "=[已隐藏]"
		}
		return "[已隐藏]"
	})
	if len(value) > 800 {
		value = value[:800] + "…"
	}
	return value
}

func (s *Store) AddAudit(entry app.AuditEntry) error {
	if strings.TrimSpace(entry.RequestID) == "" {
		entry.RequestID = app.NewAuditRequestID("audit")
	}
	if entry.CreatedAt <= 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	_, err := s.DB.Exec(`INSERT INTO audit_logs(request_id,source,action,entity_type,entity_id,summary,success,error_message,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		entry.RequestID,
		sanitizeAuditText(entry.Source),
		sanitizeAuditText(entry.Action),
		sanitizeAuditText(entry.EntityType),
		sanitizeAuditText(entry.EntityID),
		sanitizeAuditText(entry.Summary),
		map[bool]int{true: 1, false: 0}[entry.Success],
		sanitizeAuditText(entry.Error),
		entry.CreatedAt,
	)
	return err
}

func (s *Store) AuditLogs(source, action, keyword string, limit int) ([]app.AuditEntry, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	conditions := []string{"1=1"}
	args := make([]any, 0, 4)
	if source = strings.TrimSpace(source); source != "" {
		conditions = append(conditions, "source=?")
		args = append(args, source)
	}
	if action = strings.TrimSpace(action); action != "" {
		conditions = append(conditions, "action=?")
		args = append(args, action)
	}
	if keyword = strings.TrimSpace(keyword); keyword != "" {
		conditions = append(conditions, "(request_id LIKE ? OR entity_id LIKE ? OR summary LIKE ? OR error_message LIKE ?)")
		like := "%" + keyword + "%"
		args = append(args, like, like, like, like)
	}
	args = append(args, limit)
	rows, err := s.DB.Query(`SELECT request_id,source,action,entity_type,entity_id,summary,success,error_message,created_at FROM audit_logs WHERE `+strings.Join(conditions, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]app.AuditEntry, 0)
	for rows.Next() {
		var entry app.AuditEntry
		var success int
		if err := rows.Scan(&entry.RequestID, &entry.Source, &entry.Action, &entry.EntityType, &entry.EntityID, &entry.Summary, &success, &entry.Error, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entry.Success = success == 1
		items = append(items, entry)
	}
	return items, rows.Err()
}
