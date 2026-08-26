package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	AuditSourceWeb        = "web"
	AuditSourceTelegram   = "telegram"
	AuditSourceSchedule   = "schedule"
	AuditSourceRotation   = "rotation"
	AuditSourceProtection = "traffic_protection"
	AuditSourceJob        = "background_job"
)

type AuditEntry struct {
	RequestID  string `json:"request_id"`
	Source     string `json:"source"`
	Action     string `json:"action"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Summary    string `json:"summary"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

func NewAuditRequestID(prefix string) string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err == nil {
		return prefix + "-" + hex.EncodeToString(raw)
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
