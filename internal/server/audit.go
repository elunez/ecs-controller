package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Kori1c/ecs-controller/internal/app"
)

type auditResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len() < 4096 {
		_, _ = w.body.Write(value[:min(len(value), 4096-w.body.Len())])
	}
	return w.ResponseWriter.Write(value)
}

type webAuditMeta struct {
	Action     string
	EntityType string
	EntityID   string
	Summary    string
}

func webAuditMetadata(action string, data map[string]any) (webAuditMeta, bool) {
	accountID := strconv.FormatInt(int64(number(data["accountId"], number(data["id"], 0))), 10)
	if accountID == "0" {
		accountID = ""
	}
	switch action {
	case "save_config":
		return webAuditMeta{Action: "config_update", EntityType: "configuration", EntityID: "global", Summary: "保存系统、账号或分组配置"}, true
	case "create_ecs":
		return webAuditMeta{Action: "ecs_create_submit", EntityType: "ecs_task", EntityID: stringValue(data["previewId"]), Summary: "提交 ECS 创建任务"}, true
	case "control_instance":
		control := stringValue(data["action"])
		label := map[string]string{"start": "手动开机", "stop": "手动停机"}[control]
		return webAuditMeta{Action: "instance_" + control, EntityType: "instance", EntityID: accountID, Summary: label}, control == "start" || control == "stop"
	case "save_instance_schedule":
		return webAuditMeta{Action: "instance_settings_update", EntityType: "instance", EntityID: accountID, Summary: "修改实例开关机设置"}, true
	case "delete_instance":
		return webAuditMeta{Action: "instance_release_submit", EntityType: "instance", EntityID: accountID, Summary: "提交实例释放任务"}, true
	case "replace_instance_ip":
		return webAuditMeta{Action: "instance_ip_replace", EntityType: "instance", EntityID: accountID, Summary: "更换实例公网 IP"}, true
	case "refresh_account":
		return webAuditMeta{Action: "instance_refresh", EntityType: "instance", EntityID: accountID, Summary: "同步单台实例"}, true
	case "sync_account_group":
		return webAuditMeta{Action: "account_sync", EntityType: "account_group", EntityID: stringValue(data["groupKey"]), Summary: "同步账号实例清单"}, true
	case "sync_instances":
		return webAuditMeta{Action: "inventory_sync", EntityType: "inventory", EntityID: "all", Summary: "同步全部实例清单"}, true
	case "restore_schedule_block":
		return webAuditMeta{Action: "traffic_block_restore", EntityType: "instance", EntityID: accountID, Summary: "恢复流量阈值后的定时任务"}, true
	case "start_update":
		return webAuditMeta{Action: "online_update_submit", EntityType: "release", EntityID: stringValue(data["target_version"]), Summary: "提交在线更新"}, true
	case "retry_runtime_job":
		return webAuditMeta{Action: "job_retry", EntityType: "job", EntityID: stringValue(data["jobId"]), Summary: "重新执行失败任务"}, true
	case "clear_logs":
		return webAuditMeta{Action: "logs_clear", EntityType: "log", EntityID: stringValue(data["tab"]), Summary: "清空系统日志"}, true
	default:
		return webAuditMeta{}, false
	}
}

func auditResponseError(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return "请求执行失败"
	}
	return strings.TrimSpace(fallback(stringValue(payload["error"]), stringValue(payload["message"])))
}

func (s *Server) auditWebResult(requestID string, meta webAuditMeta, response *auditResponseWriter) {
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	success := status >= 200 && status < 400
	entry := app.AuditEntry{RequestID: requestID, Source: app.AuditSourceWeb, Action: meta.Action, EntityType: meta.EntityType, EntityID: meta.EntityID, Summary: meta.Summary, Success: success}
	if !success {
		entry.Error = auditResponseError(response.body.Bytes())
	}
	if err := s.Store.AddAudit(entry); err != nil && s.Log != nil {
		s.Log.Printf("保存网页操作审计失败: %v", err)
	}
}

func (s *Server) auditLogs(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.AuditLogs(r.URL.Query().Get("source"), r.URL.Query().Get("audit_action"), r.URL.Query().Get("keyword"), number(r.URL.Query().Get("limit"), 100))
	if err != nil {
		s.error(w, http.StatusInternalServerError, "操作审计读取失败")
		return
	}
	s.json(w, http.StatusOK, map[string]any{"data": items})
}
