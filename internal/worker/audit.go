package worker

import (
	"fmt"

	"github.com/Kori1c/ecs-controller/internal/app"
)

func (w *Worker) audit(source, action, entityType, entityID, summary string, err error) {
	entry := app.AuditEntry{
		RequestID:  app.NewAuditRequestID(source),
		Source:     source,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		Summary:    summary,
		Success:    err == nil,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	if auditErr := w.Store.AddAudit(entry); auditErr != nil && w.Log != nil {
		w.Log.Printf("保存操作审计失败: %v", auditErr)
	}
}

func auditAccountID(account app.Account) string {
	if account.ID > 0 {
		return fmt.Sprint(account.ID)
	}
	return account.InstanceID
}
