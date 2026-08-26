package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

const (
	runtimeInstanceMonitor = "instance_monitor"
	runtimeInventorySync   = "inventory_sync"
	runtimeRotation        = "rotation_ddns"
	runtimeDailySummary    = "daily_summary"
	runtimeJobQueue        = "job_queue"
)

var runtimeComponents = map[string]struct {
	label string
	order int
}{
	runtimeInstanceMonitor: {label: "实例状态与流量监控", order: 10},
	runtimeInventorySync:   {label: "账号实例清单同步", order: 20},
	runtimeRotation:        {label: "分组轮转与 DDNS", order: 30},
	runtimeDailySummary:    {label: "每日通知摘要", order: 40},
	runtimeJobQueue:        {label: "后台任务队列", order: 50},
}

func (w *Worker) setRuntimeWaiting(key, detail string, nextRun time.Time) {
	item, _ := w.Store.RuntimeStatus(key)
	meta := runtimeComponents[key]
	item.Key, item.Label, item.Status, item.Detail = key, meta.label, "waiting", detail
	if !nextRun.IsZero() {
		item.NextRunAt = nextRun.Unix()
	}
	_ = w.Store.SetRuntimeStatus(item, meta.order)
}

func (w *Worker) beginRuntime(key, detail string, nextRun time.Time) time.Time {
	started := time.Now()
	item, _ := w.Store.RuntimeStatus(key)
	meta := runtimeComponents[key]
	item.Key, item.Label, item.Status = key, meta.label, "running"
	item.LastStartedAt, item.Detail = started.Unix(), detail
	if !nextRun.IsZero() {
		item.NextRunAt = nextRun.Unix()
	}
	_ = w.Store.SetRuntimeStatus(item, meta.order)
	return started
}

func (w *Worker) finishRuntime(key string, started time.Time, detail string, nextRun time.Time, runErr error) {
	now := time.Now()
	item, _ := w.Store.RuntimeStatus(key)
	meta := runtimeComponents[key]
	item.Key, item.Label, item.Detail = key, meta.label, detail
	item.LastDurationMS = now.Sub(started).Milliseconds()
	if !nextRun.IsZero() {
		item.NextRunAt = nextRun.Unix()
	}
	if runErr != nil {
		item.Status, item.LastFailureAt, item.LastError = "error", now.Unix(), sanitizeRuntimeError(runErr.Error())
	} else {
		item.Status, item.LastSuccessAt, item.LastError = "ok", now.Unix(), ""
	}
	_ = w.Store.SetRuntimeStatus(item, meta.order)
}

func sanitizeRuntimeError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 500 {
		return message[:500] + "…"
	}
	return message
}

// InventoryMonitor periodically reconciles the complete ECS list for every
// configured account group. It runs independently from the per-instance
// status/traffic monitor so a slow list operation cannot delay automation.
func (w *Worker) InventoryMonitor(ctx context.Context, checkInterval time.Duration) {
	if checkInterval < 30*time.Second {
		checkInterval = 30 * time.Second
	}
	w.runInventorySync(ctx, time.Now())
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.runInventorySync(ctx, now)
		}
	}
}

func (w *Worker) runInventorySync(ctx context.Context, now time.Time) bool {
	intervalSeconds, _ := strconv.ParseInt(w.Store.GetSetting("inventory_sync_interval", "3600"), 10, 64)
	if intervalSeconds < 1800 || intervalSeconds > 86400 {
		intervalSeconds = 3600
	}
	lastAttempt, _ := strconv.ParseInt(w.Store.GetSetting("inventory_sync_last_attempt", "0"), 10, 64)
	nextRun := time.Unix(lastAttempt+intervalSeconds, 0)
	if w.InventorySync == nil || w.Store.GetSetting("inventory_sync_enabled", "1") != "1" {
		w.setRuntimeDisabled(runtimeInventorySync, "自动同步已关闭")
		return false
	}
	if lastAttempt > 0 && now.Unix()-lastAttempt < intervalSeconds {
		w.setRuntimeWaiting(runtimeInventorySync, "等待下次账号清单同步", nextRun)
		return false
	}
	_ = w.Store.SetSetting("inventory_sync_last_attempt", strconv.FormatInt(now.Unix(), 10))
	started := w.beginRuntime(runtimeInventorySync, "正在同步全部账号组", now.Add(time.Duration(intervalSeconds)*time.Second))
	count, err := w.InventorySync(ctx)
	if err != nil {
		failures, _ := strconv.Atoi(w.Store.GetSetting("inventory_sync_failures", "0"))
		failures++
		_ = w.Store.SetSetting("inventory_sync_failures", strconv.Itoa(failures))
		w.Store.AddLog("warning", "账号实例清单自动同步失败: "+err.Error())
		w.finishRuntime(runtimeInventorySync, started, fmt.Sprintf("连续失败 %d 次", failures), now.Add(time.Duration(intervalSeconds)*time.Second), err)
		if failures == 3 {
			w.dispatchEvent(ctx, notify.Event{Title: "账号实例清单同步失败", Summary: "已连续 3 次同步失败，请检查阿里云凭据和网络。", Text: "【ECS 控制台】账号实例清单自动同步已连续 3 次失败\n错误: " + err.Error(), Fields: map[string]string{"error": err.Error()}})
		}
		w.audit(app.AuditSourceSchedule, "inventory_sync", "inventory", "all", "自动同步全部账号实例清单", err)
		return true
	}
	_ = w.Store.SetSetting("inventory_sync_failures", "0")
	_ = w.Store.SetSetting("inventory_sync_last_success", strconv.FormatInt(now.Unix(), 10))
	detail := fmt.Sprintf("已发现 %d 台实例", count)
	w.finishRuntime(runtimeInventorySync, started, detail, now.Add(time.Duration(intervalSeconds)*time.Second), nil)
	w.Store.AddLog("info", fmt.Sprintf("账号实例清单自动同步完成：共发现 %d 台实例。", count))
	w.audit(app.AuditSourceSchedule, "inventory_sync", "inventory", "all", fmt.Sprintf("自动同步完成，共发现 %d 台实例", count), nil)
	return true
}

func (w *Worker) setRuntimeDisabled(key, detail string) {
	item, _ := w.Store.RuntimeStatus(key)
	meta := runtimeComponents[key]
	item.Key, item.Label, item.Status, item.Detail = key, meta.label, "disabled", detail
	item.NextRunAt, item.LastError = 0, ""
	_ = w.Store.SetRuntimeStatus(item, meta.order)
}

func (w *Worker) updateDependentRuntime(now time.Time) {
	groups, groupErr := w.Store.LoadRotationGroups()
	if groupErr != nil {
		started := w.beginRuntime(runtimeRotation, "读取分组状态", time.Time{})
		w.finishRuntime(runtimeRotation, started, "分组状态读取失败", time.Time{}, groupErr)
	} else if len(groups) == 0 {
		w.setRuntimeDisabled(runtimeRotation, "没有启用轮转分组")
	} else {
		var failures int
		for _, group := range groups {
			states, err := w.Store.RotationStates(group.ID)
			if err != nil {
				failures++
				continue
			}
			for _, state := range states {
				if state.LastError != "" {
					failures++
				}
			}
		}
		started := w.beginRuntime(runtimeRotation, fmt.Sprintf("%d 个轮转分组", len(groups)), now.Add(time.Minute))
		if failures > 0 {
			w.finishRuntime(runtimeRotation, started, fmt.Sprintf("%d 个分组或实例存在错误", failures), now.Add(time.Minute), fmt.Errorf("轮转或 DDNS 最近执行存在错误"))
		} else {
			w.finishRuntime(runtimeRotation, started, fmt.Sprintf("%d 个轮转分组运行正常", len(groups)), now.Add(time.Minute), nil)
		}
	}

	if w.Store.GetSetting(dailyTrafficEnabledSetting, "0") != "1" {
		w.setRuntimeDisabled(runtimeDailySummary, "每日摘要未启用")
		return
	}
	reportTime, err := time.ParseInLocation("15:04", w.Store.GetSetting(dailyTrafficTimeSetting, "00:00"), now.Location())
	if err != nil {
		started := w.beginRuntime(runtimeDailySummary, "摘要时间配置无效", time.Time{})
		w.finishRuntime(runtimeDailySummary, started, "请重新设置推送时间", time.Time{}, err)
		return
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), reportTime.Hour(), reportTime.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	w.setRuntimeWaiting(runtimeDailySummary, "等待每日摘要推送", next)
}
