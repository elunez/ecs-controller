package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/notify"
	"github.com/Kori1c/ecs-controller/internal/store"
)

type Worker struct {
	Store                   *store.Store
	CloudFactory            func(app.AccountGroup) cloud.Client
	InventorySync           func(context.Context) (int, error)
	Log                     *log.Logger
	telegramConflictLastLog int64
}

func (w *Worker) cloudClient(group app.AccountGroup) cloud.Client {
	if w.CloudFactory == nil {
		return nil
	}
	return w.CloudFactory(group)
}

func cmsTrafficErrorMessage(err error) string {
	if cloud.IsMetricNoDataError(err) {
		return "云端数据尚未更新，请稍后再试"
	}
	return "CMS 实例流量暂不可用: " + err.Error()
}

func (w *Worker) Monitor(ctx context.Context, interval time.Duration) {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	w.Store.SetLastRun()
	w.setRuntimeWaiting(runtimeInstanceMonitor, "后台监控已启动", time.Now())
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			w.runMonitorCycle(ctx, interval)
			timer.Reset(interval)
		}
	}
}

func (w *Worker) runMonitorCycle(ctx context.Context, interval time.Duration) {
	w.Store.SetLastRun()
	now := time.Now()
	started := w.beginRuntime(runtimeInstanceMonitor, "正在检查实例状态与流量", now.Add(interval))
	if err := w.Store.ResetMonthlyTraffic(); err != nil {
		w.Store.AddLog("warning", "月度流量状态重置失败: "+err.Error())
	}
	maintenanceDay := now.Format("2006-01-02")
	if w.Store.GetSetting("maintenance_day", "") != maintenanceDay {
		if err := w.Store.PruneMaintenance(now); err != nil {
			w.Store.AddLog("warning", "数据库历史清理失败: "+err.Error())
		}
		if now.Hour() == 4 {
			if err := w.Store.Vacuum(); err != nil {
				w.Store.AddLog("warning", "数据库 VACUUM 失败: "+err.Error())
			}
		}
		_ = w.Store.SetSetting("maintenance_day", maintenanceDay)
	}
	accounts, err := w.Store.LoadAccounts(false)
	if err != nil {
		w.Store.AddLog("error", "读取监控账号失败: "+err.Error())
		w.finishRuntime(runtimeInstanceMonitor, started, "实例列表读取失败", time.Now().Add(interval), err)
		return
	}
	threshold, _ := strconv.ParseFloat(w.Store.GetSetting("traffic_threshold", "95"), 64)
	if threshold <= 0 || threshold > 100 {
		threshold = 95
	}
	thresholdAction := w.Store.GetSetting("threshold_action", "stop_and_notify")
	shutdownMode := w.Store.GetSetting("shutdown_mode", "KeepCharging")
	keepAlive := w.Store.GetSetting("keep_alive", "0") == "1"
	monthlyAutoStart := w.Store.GetSetting("monthly_auto_start", "0") == "1"
	enableBilling := w.Store.GetSetting("enable_billing", "0") == "1"
	apiInterval, _ := strconv.ParseInt(w.Store.GetSetting("api_interval", "600"), 10, 64)
	if apiInterval < 30 {
		apiInterval = 600
	}

	grouped := make(map[string][]app.Account)
	groupOrder := make([]string, 0)
	for _, account := range accounts {
		if account.InstanceID == "" {
			continue
		}
		key := account.GroupKey
		if key == "" {
			key = account.AccessKeyID + "|" + account.RegionID
		}
		if _, exists := grouped[key]; !exists {
			groupOrder = append(groupOrder, key)
		}
		grouped[key] = append(grouped[key], account)
	}

	semaphore := make(chan struct{}, 3)
	results := make(chan error, len(accounts))
	var wg sync.WaitGroup
	for _, key := range groupOrder {
		groupAccounts := grouped[key]
		wg.Add(1)
		go func(items []app.Account) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			for _, account := range items {
				accountCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				runErr := w.monitorAccount(accountCtx, account, now, apiInterval, threshold, thresholdAction, shutdownMode, keepAlive, monthlyAutoStart, enableBilling)
				cancel()
				results <- runErr
			}
		}(groupAccounts)
	}
	wg.Wait()
	close(results)
	failures := 0
	for runErr := range results {
		if runErr != nil {
			failures++
		}
	}
	w.runDailyTrafficSummary(ctx, now)
	w.runBillingAlerts(ctx, now)
	w.updateDependentRuntime(now)
	detail := fmt.Sprintf("%d 台实例，%d 个账号组", len(accounts), len(groupOrder))
	var cycleErr error
	if failures > 0 {
		detail += fmt.Sprintf("，%d 台检查异常", failures)
		cycleErr = fmt.Errorf("%d 台实例检查异常", failures)
	}
	w.Store.AddLog("heartbeat", "监控心跳正常："+detail+"。")
	w.finishRuntime(runtimeInstanceMonitor, started, detail, time.Now().Add(interval), cycleErr)
}

func (w *Worker) monitorAccount(ctx context.Context, account app.Account, now time.Time, apiInterval int64, threshold float64, thresholdAction, shutdownMode string, keepAlive, monthlyAutoStart, enableBilling bool) error {
	client := w.cloudClient(app.AccountGroup{AccessKeyID: account.AccessKeyID, AccessKeySecret: account.AccessKeySecret, RegionID: account.RegionID, SiteType: account.SiteType})
	if client == nil {
		return fmt.Errorf("实例 %s 的云客户端未配置", account.InstanceID)
	}
	transient := account.InstanceStatus == "Starting" || account.InstanceStatus == "Stopping" || account.InstanceStatus == "Pending" || account.InstanceStatus == "Unknown"
	shouldRefresh := account.UpdatedAt <= 0 || now.Unix()-account.UpdatedAt >= apiInterval || transient || now.Minute() == 0
	if !shouldRefresh {
		w.runCachedAutomation(ctx, client, &account, now, threshold, thresholdAction, shutdownMode, keepAlive, monthlyAutoStart)
		return ctx.Err()
	}
	oldPublicIP := account.PublicIP
	oldStatus := account.InstanceStatus
	instance, describeErr := client.DescribeInstance(ctx, account.RegionID, account.InstanceID)
	if describeErr != nil {
		if removed, cleanupErr := w.queueMissingInstanceCleanup(account, describeErr); cleanupErr != nil {
			w.Store.AddLog("warning", "清理云端不存在的实例记录失败: "+cleanupErr.Error())
			return cleanupErr
		} else if removed {
			return nil
		}
		metadata := map[string]any{"health_status": "error", "traffic_api_status": "unknown", "traffic_api_message": describeErr.Error()}
		if cloud.IsCredentialError(describeErr) {
			metadata["protection_suspended"] = true
			metadata["protection_suspend_reason"] = "credential_invalid"
			if account.ProtectionNotifiedAt == 0 {
				w.dispatchEvent(ctx, notify.Event{Title: "阿里云凭据异常", Summary: "已暂停自动停机保护", AccountID: accountLabel(account), Text: fmt.Sprintf("【ECS 控制台】阿里云凭据异常\n账号/实例: %s\n实例 ID: %s\n区域: %s\n错误: %s\n系统已暂停自动停机保护，请更新 AK 后恢复。", accountLabel(account), account.InstanceID, account.RegionID, describeErr.Error()), Fields: map[string]string{"instance_id": account.InstanceID, "reason": "credential_invalid"}})
				account.ProtectionNotifiedAt = now.Unix()
				metadata["protection_suspend_notified_at"] = account.ProtectionNotifiedAt
			}
		}
		_ = w.Store.UpdateAccountStatus(account.ID, account.TrafficUsed, account.InstanceStatus, now.Unix(), metadata)
		return describeErr
	}
	account.InstanceStatus, account.PublicIP, account.PrivateIP, account.InstanceType = instance.Status, instance.PublicIP, instance.PrivateIP, instance.InstanceType
	if account.PublicIPMode == "eip" && account.EIPAddress != "" {
		account.PublicIP = account.EIPAddress
	}
	account.HealthStatus, account.UpdatedAt = "ok", now.Unix()
	if oldStatus != "" && oldStatus != account.InstanceStatus {
		w.dispatchEvent(ctx, statusEvent(account, oldStatus, account.InstanceStatus, "系统监控检测到实例状态变化。"))
	}
	traffic, trafficStatus, trafficMessage, trafficErr := w.refreshTraffic(ctx, client, account, now)
	if trafficErr != nil {
		reason := "traffic_api_error"
		if cloud.IsCredentialError(trafficErr) {
			reason = "credential_invalid"
		}
		account.TrafficAPIStatus, account.TrafficAPIMessage = "error", trafficMessage
		account.ProtectionSuspended, account.ProtectionSuspendReason = true, reason
		account.UpdatedAt, account.TrafficBillingMonth = now.Unix(), now.Format("2006-01")
		if available := w.applyTrafficProtection(ctx, client, &account, now, threshold, thresholdAction, shutdownMode, account.TrafficUsed, false, keepAlive, monthlyAutoStart); available {
			account.ProtectionSuspended, account.ProtectionSuspendReason = false, ""
		}
		if reason == "credential_invalid" && account.ProtectionNotifiedAt == 0 {
			w.dispatchEvent(ctx, notify.Event{Title: "阿里云凭据异常", Summary: "已暂停自动停机保护", AccountID: accountLabel(account), Text: fmt.Sprintf("【ECS 控制台】阿里云凭据异常\n账号/实例: %s\n实例 ID: %s\n错误: %s\n系统已暂停自动停机保护，请更新 AK 后恢复。", accountLabel(account), account.InstanceID, trafficErr.Error()), Fields: map[string]string{"instance_id": account.InstanceID, "reason": "credential_invalid"}})
			account.ProtectionNotifiedAt = now.Unix()
		}
		if err := w.Store.UpsertAccount(account); err != nil {
			w.Store.AddLog("error", "保存流量保护状态失败: "+err.Error())
			return err
		}
		return trafficErr
	}
	account.TrafficUsed = traffic
	account.TrafficBillingMonth = now.Format("2006-01")
	account.TrafficAPIStatus, account.TrafficAPIMessage, account.ProtectionSuspended, account.ProtectionSuspendReason = trafficStatus, trafficMessage, false, ""
	if account.ProtectionNotifiedAt != 0 {
		account.ProtectionNotifiedAt = 0
	}
	_ = w.Store.AddTrafficHistory(account.ID, traffic, now)
	if enableBilling && account.InstanceID != "" {
		cycle := now.Format("2006-01")
		if billingClient, ok := client.(cloud.BillingClient); ok {
			if _, cacheOK := w.Store.GetBillingCache(account.ID, "balance", "", 6*time.Hour); !cacheOK {
				if balance, currency, balanceErr := billingClient.GetAccountBalance(ctx, account.SiteType); balanceErr == nil {
					_ = w.Store.SetBillingCache(account.ID, "balance", "", map[string]any{"balance": balance, "currency": currency})
				} else {
					_ = w.Store.SetBillingCache(account.ID, "balance", "", map[string]any{"error": balanceErr.Error()})
				}
			}
			if _, cacheOK := w.Store.GetBillingCache(account.ID, "bill_overview", cycle, 6*time.Hour); !cacheOK {
				if total, currency, overviewErr := billingClient.GetBillOverview(ctx, account.SiteType, cycle); overviewErr == nil {
					_ = w.Store.SetBillingCache(account.ID, "bill_overview", cycle, map[string]any{"monthly_cost": total, "currency": currency})
				} else {
					_ = w.Store.SetBillingCache(account.ID, "bill_overview", cycle, map[string]any{"error": overviewErr.Error()})
				}
			}
		}
		if _, ok := w.Store.GetBillingCache(account.ID, "instance_bill", cycle, 6*time.Hour); !ok {
			balance, monthlyCost, currency, billingErr := client.GetBilling(ctx, account.SiteType, account.InstanceID, cycle)
			if billingErr != nil {
				_ = w.Store.SetBillingCache(account.ID, "instance_bill", cycle, map[string]any{"error": billingErr.Error()})
			} else {
				_ = w.Store.SetBillingCache(account.ID, "instance_bill", cycle, map[string]any{"monthly_cost": monthlyCost, "balance": balance, "currency": currency})
			}
		}
	}
	w.applyTrafficProtection(ctx, client, &account, now, threshold, thresholdAction, shutdownMode, traffic, true, keepAlive, monthlyAutoStart)
	if err := w.Store.UpsertAccount(account); err != nil {
		w.Store.AddLog("error", "保存监控状态失败: "+err.Error())
		return err
	}
	if account.PublicIP != "" && account.PublicIP != oldPublicIP {
		w.dispatchEvent(ctx, notify.Event{Title: "公网 IP 已变化", Summary: fmt.Sprintf("%s 公网 IP 已从 %s 变为 %s", accountLabel(account), oldPublicIP, account.PublicIP), AccountID: accountLabel(account), Text: fmt.Sprintf("【ECS 控制台】公网 IP 已变化\n实例: %s\n实例 ID: %s\n旧 IP: %s\n新 IP: %s\n时间: %s", accountLabel(account), account.InstanceID, oldPublicIP, account.PublicIP, time.Now().Format("2006-01-02 15:04:05")), Fields: map[string]string{"old_ip": oldPublicIP, "new_ip": account.PublicIP, "instance_id": account.InstanceID}})
	}
	return ctx.Err()
}

// queueMissingInstanceCleanup hides a stale row as soon as the per-instance
// API confirms the exact ECS ID no longer exists. Remaining managed-resource
// and rotation-group cleanup is performed by the retryable delete job.
func (w *Worker) queueMissingInstanceCleanup(account app.Account, describeErr error) (bool, error) {
	if !cloud.IsNotFound(describeErr) {
		return false, nil
	}
	if err := w.Store.QueueMissingInstanceCleanup(account.ID, randomActionToken()); err != nil {
		return false, err
	}
	w.Store.AddLog("info", "已同步移除云端不存在的实例: "+account.InstanceID)
	return true, nil
}

func (w *Worker) protectionTraffic(ctx context.Context, client cloud.Client, account app.Account, cmsTraffic float64) (float64, string) {
	traffic, source, _ := w.protectionTrafficStatus(ctx, client, account, cmsTraffic, true)
	return traffic, source
}

// protectionTrafficStatus combines the independent CMS and CDT signals. CMS
// remains the value shown on an instance card, while the larger available
// value is used for the safety threshold. If both APIs are unavailable,
// automation must pause instead of trusting an old value.
func (w *Worker) protectionTrafficStatus(ctx context.Context, client cloud.Client, account app.Account, cmsTraffic float64, cmsAvailable bool) (float64, string, bool) {
	cycle := time.Now().Format("2006-01")
	if account.ID > 0 {
		if cached, ok := w.Store.GetBillingCache(account.ID, "cdt_protection", cycle, 5*time.Minute); ok {
			cdtTraffic := trafficFloat(cached["traffic"])
			if !cmsAvailable {
				return cdtTraffic, "CDT", true
			}
			if cdtTraffic >= cmsTraffic {
				return cdtTraffic, "CDT", true
			}
			return cmsTraffic, "CMS", true
		}
	}
	cdtTraffic, err := client.GetTraffic(ctx, account.RegionID)
	if err == nil {
		if account.ID > 0 {
			_ = w.Store.SetBillingCache(account.ID, "cdt_protection", cycle, map[string]any{"traffic": cdtTraffic})
		}
		if !cmsAvailable || cdtTraffic >= cmsTraffic {
			return cdtTraffic, "CDT", true
		}
		return cmsTraffic, "CMS", true
	}
	if cmsAvailable {
		return cmsTraffic, "CMS", true
	}
	return 0, "", false
}

func trafficFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case string:
		parsed, _ := strconv.ParseFloat(typed, 64)
		return parsed
	default:
		return 0
	}
}

func (w *Worker) runCachedAutomation(ctx context.Context, client cloud.Client, account *app.Account, now time.Time, threshold float64, thresholdAction, shutdownMode string, keepAlive, monthlyAutoStart bool) {
	shutdownMode = app.ResolveShutdownMode(account.StoppedMode, shutdownMode)
	rotationManaged := w.rotationManaged(account.ID)
	protectionTraffic, protectionSource, available := w.protectionTrafficStatus(ctx, client, *account, account.TrafficUsed, account.TrafficAPIStatus != "error" && !account.ProtectionSuspended)
	if !available {
		account.ProtectionSuspended = true
		if account.ProtectionSuspendReason == "" {
			account.ProtectionSuspendReason = "traffic_api_error"
		}
		if err := w.Store.UpsertAccount(*account); err != nil {
			w.Store.AddLog("error", "保存流量保护暂停状态失败: "+err.Error())
		}
		return
	}
	account.ProtectionSuspended, account.ProtectionSuspendReason = false, ""
	usagePercent := 0.0
	if account.MaxTraffic > 0 {
		usagePercent = protectionTraffic / account.MaxTraffic * 100
	}
	requiresProtection := account.MaxTraffic > 0 && usagePercent >= threshold
	if requiresProtection {
		if thresholdAction == "stop_and_notify" && account.InstanceStatus == "Running" {
			if err := client.StopInstance(ctx, account.RegionID, account.InstanceID, shutdownMode); err != nil {
				w.Store.AddLog("error", "流量阈值停机失败: "+err.Error())
				w.audit(app.AuditSourceProtection, "traffic_threshold_stop", "instance", auditAccountID(*account), "流量达到阈值，提交停机", err)
			} else {
				old := account.InstanceStatus
				account.InstanceStatus = "Stopping"
				account.ScheduleBlockedByTraffic = true
				_ = w.Store.SetGroupScheduleBlocked(account.GroupKey, true)
				w.Store.AddLog("warning", fmt.Sprintf("实例已达到流量保护阈值，已发起停机: %s (%.2f%%, 来源: %s)", account.InstanceID, usagePercent, protectionSource))
				w.dispatchEvent(ctx, statusEvent(*account, old, "Stopping", fmt.Sprintf("%s 流量达到保护阈值，已提交停机。", protectionSource)))
				w.dispatchEvent(ctx, trafficEvent(*account, protectionTraffic, usagePercent, threshold, fmt.Sprintf("已达到阈值，已提交停机（来源：%s）", protectionSource)))
				account.ProtectionNotifiedAt = now.Unix()
				w.audit(app.AuditSourceProtection, "traffic_threshold_stop", "instance", auditAccountID(*account), fmt.Sprintf("流量使用率 %.2f%%，已提交停机", usagePercent), nil)
			}
		} else if thresholdAction == "notify_only" && (account.ProtectionNotifiedAt == 0 || now.Unix()-account.ProtectionNotifiedAt >= 6*60*60) {
			w.dispatchEvent(ctx, trafficEvent(*account, protectionTraffic, usagePercent, threshold, fmt.Sprintf("仅发送告警（来源：%s）", protectionSource)))
			account.ProtectionNotifiedAt = now.Unix()
		}
	} else if !account.ScheduleBlockedByTraffic {
		if rotationManaged {
			w.runRotationSchedule(ctx, client, account, now, shutdownMode)
		} else {
			w.runSchedule(ctx, client, account, now, shutdownMode)
		}
		if !rotationManaged && monthlyAutoStart && now.Day() == 1 && account.InstanceStatus == "Stopped" && !account.AutoStartBlocked && !scheduledStopBlocksAutomaticStart(*account) && !sameMonth(account.LastKeepAliveAt, now) {
			if err := client.StartInstance(ctx, account.RegionID, account.InstanceID); err == nil {
				account.InstanceStatus = "Starting"
				account.LastKeepAliveAt = now.Unix()
				w.dispatchEvent(ctx, statusEvent(*account, "Stopped", "Starting", "每月 1 号自动开机。"))
			}
		}
		if !rotationManaged && keepAlive && account.InstanceStatus == "Stopped" && canKeepAlive(*account, requiresProtection) {
			if err := client.StartInstance(ctx, account.RegionID, account.InstanceID); err == nil {
				account.InstanceStatus = "Starting"
				account.LastKeepAliveAt = now.Unix()
				w.dispatchEvent(ctx, statusEvent(*account, "Stopped", "Starting", "检测到实例停机，保活已提交开机。"))
			}
		}
	}
	if err := w.Store.UpsertAccount(*account); err != nil {
		w.Store.AddLog("error", "保存缓存自动化状态失败: "+err.Error())
	}
}

// applyTrafficProtection runs the same threshold and auto-start decisions
// after a fresh CMS sample or after CMS failed and CDT supplied the fallback.
func (w *Worker) applyTrafficProtection(ctx context.Context, client cloud.Client, account *app.Account, now time.Time, threshold float64, thresholdAction, shutdownMode string, cmsTraffic float64, cmsAvailable, keepAlive, monthlyAutoStart bool) bool {
	shutdownMode = app.ResolveShutdownMode(account.StoppedMode, shutdownMode)
	rotationManaged := w.rotationManaged(account.ID)
	protectionTraffic, protectionSource, available := w.protectionTrafficStatus(ctx, client, *account, cmsTraffic, cmsAvailable)
	if !available {
		account.ProtectionSuspended = true
		if account.ProtectionSuspendReason == "" {
			account.ProtectionSuspendReason = "traffic_api_error"
		}
		return false
	}
	account.ProtectionSuspended, account.ProtectionSuspendReason = false, ""
	usagePercent := 0.0
	if account.MaxTraffic > 0 {
		usagePercent = protectionTraffic / account.MaxTraffic * 100
	}
	requiresProtection := account.MaxTraffic > 0 && usagePercent >= threshold
	protectionAction := ""
	if requiresProtection && thresholdAction == "stop_and_notify" && account.InstanceStatus == "Running" {
		if err := client.StopInstance(ctx, account.RegionID, account.InstanceID, shutdownMode); err != nil {
			w.Store.AddLog("error", "流量阈值停机失败: "+err.Error())
			w.audit(app.AuditSourceProtection, "traffic_threshold_stop", "instance", auditAccountID(*account), "流量达到阈值，提交停机", err)
		} else {
			old := account.InstanceStatus
			account.InstanceStatus = "Stopping"
			account.ScheduleBlockedByTraffic = true
			_ = w.Store.SetGroupScheduleBlocked(account.GroupKey, true)
			w.Store.AddLog("warning", fmt.Sprintf("实例已达到流量保护阈值，已发起停机: %s (%.2f%%, 来源: %s)", account.InstanceID, usagePercent, protectionSource))
			w.dispatchEvent(ctx, statusEvent(*account, old, "Stopping", fmt.Sprintf("%s 流量达到保护阈值，已提交停机。", protectionSource)))
			protectionAction = fmt.Sprintf("已达到阈值，已提交停机（来源：%s）", protectionSource)
			w.audit(app.AuditSourceProtection, "traffic_threshold_stop", "instance", auditAccountID(*account), fmt.Sprintf("流量使用率 %.2f%%，已提交停机", usagePercent), nil)
		}
	}
	if requiresProtection && thresholdAction == "notify_only" {
		protectionAction = fmt.Sprintf("仅发送告警（来源：%s）", protectionSource)
	}
	if protectionAction != "" && (account.ProtectionNotifiedAt == 0 || now.Unix()-account.ProtectionNotifiedAt >= 6*60*60) {
		w.dispatchEvent(ctx, trafficEvent(*account, protectionTraffic, usagePercent, threshold, protectionAction))
		account.ProtectionNotifiedAt = now.Unix()
	}
	if cmsAvailable && !requiresProtection && !account.ScheduleBlockedByTraffic {
		if rotationManaged {
			w.runRotationSchedule(ctx, client, account, now, shutdownMode)
		} else {
			w.runSchedule(ctx, client, account, now, shutdownMode)
		}
		if !rotationManaged && monthlyAutoStart && now.Day() == 1 && account.InstanceStatus == "Stopped" && !account.AutoStartBlocked && !scheduledStopBlocksAutomaticStart(*account) && !sameMonth(account.LastKeepAliveAt, now) {
			if err := client.StartInstance(ctx, account.RegionID, account.InstanceID); err == nil {
				account.InstanceStatus = "Starting"
				account.LastKeepAliveAt = now.Unix()
				w.dispatchEvent(ctx, statusEvent(*account, "Stopped", "Starting", "每月 1 号自动开机。"))
			} else {
				w.Store.AddLog("error", "月初自动开机失败: "+err.Error())
			}
		}
		if !rotationManaged && keepAlive && account.InstanceStatus == "Stopped" && canKeepAlive(*account, requiresProtection) {
			if err := client.StartInstance(ctx, account.RegionID, account.InstanceID); err == nil {
				account.InstanceStatus = "Starting"
				account.LastKeepAliveAt = now.Unix()
				w.Store.AddLog("info", "保活已启动实例: "+account.InstanceID)
				w.dispatchEvent(ctx, statusEvent(*account, "Stopped", "Starting", "检测到实例停机，保活已提交开机。"))
			} else {
				w.Store.AddLog("error", "保活启动失败: "+err.Error())
			}
		}
	}
	return true
}

func (w *Worker) refreshTraffic(ctx context.Context, client cloud.Client, account app.Account, now time.Time) (float64, string, string, error) {
	month := now.Format("2006-01")
	sample, err := w.Store.InstanceTrafficUsage(account.ID, account.InstanceID, month)
	if err != nil {
		return 0, "error", err.Error(), err
	}
	endMS := now.UnixMilli()
	if monthlyClient, ok := client.(cloud.MonthlyTrafficClient); ok {
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).UnixMilli()
		if monthlyBytes, points, monthlyErr := monthlyClient.GetInstanceMonthlyTraffic(ctx, account.RegionID, account.InstanceID, account.PublicIP, monthStart, endMS); monthlyErr == nil && points > 0 {
			sample, err = w.Store.SetInstanceTraffic(account.ID, account.InstanceID, month, monthlyBytes, endMS)
			if err != nil {
				return 0, "error", err.Error(), err
			}
			return sample.TrafficBytes / (1024 * 1024 * 1024), "ok", "", nil
		}
	}
	startMS := sample.LastSampleMS
	if startMS <= 0 || startMS >= endMS {
		startMS = endMS - int64(10*time.Minute/time.Millisecond)
	}
	delta, lastMS, points, _, metricErr := client.GetOutboundTrafficDelta(ctx, account.RegionID, account.InstanceID, account.PublicIP, startMS, endMS)
	if metricErr != nil {
		return 0, "error", cmsTrafficErrorMessage(metricErr), metricErr
	}
	if points > 0 {
		sample, err = w.Store.AddInstanceTraffic(account.ID, account.InstanceID, month, delta, lastMS)
		if err != nil {
			return 0, "error", err.Error(), err
		}
	}
	return sample.TrafficBytes / (1024 * 1024 * 1024), "ok", "", nil
}

func (w *Worker) runSchedule(ctx context.Context, client cloud.Client, account *app.Account, now time.Time, shutdownMode string) {
	shutdownMode = app.ResolveShutdownMode(account.StoppedMode, shutdownMode)
	if !account.ScheduleEnabled {
		return
	}

	today := now.Format("2006-01-02")
	stopHandled := false
	if account.ScheduleStopEnabled && scheduleDue(now, account.StopTime, account.ScheduleLastStopDate) {
		if account.InstanceStatus == "Running" {
			if err := client.StopInstance(ctx, account.RegionID, account.InstanceID, shutdownMode); err == nil {
				account.InstanceStatus = "Stopping"
				account.ScheduleStopActive = true
				account.ScheduleLastStopDate = today
				stopHandled = true
				_ = w.Store.UpdateScheduleExecutionState(account.ID, "stop", today)
				w.Store.AddLog("info", "执行定时停机: "+account.InstanceID)
				w.dispatchEvent(ctx, statusEvent(*account, "Running", "Stopping", "已按计划时间执行定时停机。"))
				w.audit(app.AuditSourceSchedule, "scheduled_stop", "instance", auditAccountID(*account), "按实例计划执行定时停机", nil)
			} else {
				w.audit(app.AuditSourceSchedule, "scheduled_stop", "instance", auditAccountID(*account), "按实例计划执行定时停机", err)
			}
		} else if account.InstanceStatus == "Stopped" || account.InstanceStatus == "Stopping" {
			account.ScheduleStopActive = true
			account.ScheduleLastStopDate = today
			stopHandled = true
			_ = w.Store.UpdateScheduleExecutionState(account.ID, "stop", today)
		}
	}
	// If the controller missed today's start window, do not catch up by
	// starting the instance after today's scheduled stop has already run.
	if stopHandled && account.ScheduleStartEnabled && scheduleDue(now, account.StartTime, account.ScheduleLastStartDate) {
		account.ScheduleLastStartDate = today
		_ = w.Store.UpdateScheduleExecutionState(account.ID, "start", today)
	}
	if account.ScheduleStartEnabled && scheduleDue(now, account.StartTime, account.ScheduleLastStartDate) {
		if account.InstanceStatus == "Stopped" {
			if err := client.StartInstance(ctx, account.RegionID, account.InstanceID); err == nil {
				account.InstanceStatus = "Starting"
				account.ScheduleStopActive = false
				account.ScheduleLastStartDate = today
				_ = w.Store.UpdateScheduleExecutionState(account.ID, "start", today)
				w.Store.AddLog("info", "执行定时开机: "+account.InstanceID)
				w.dispatchEvent(ctx, statusEvent(*account, "Stopped", "Starting", "已按计划时间执行定时开机。"))
				w.audit(app.AuditSourceSchedule, "scheduled_start", "instance", auditAccountID(*account), "按实例计划执行定时开机", nil)
			} else {
				w.audit(app.AuditSourceSchedule, "scheduled_start", "instance", auditAccountID(*account), "按实例计划执行定时开机", err)
			}
		} else if account.InstanceStatus == "Running" {
			account.ScheduleStopActive = false
			account.ScheduleLastStartDate = today
			_ = w.Store.UpdateScheduleExecutionState(account.ID, "start", today)
		}
	}
}

func scheduleDue(now time.Time, configured, lastDate string) bool {
	if configured == "" || lastDate == now.Format("2006-01-02") {
		return false
	}
	t, err := time.ParseInLocation("15:04", configured, now.Location())
	if err != nil {
		return false
	}
	return now.Hour() > t.Hour() || (now.Hour() == t.Hour() && now.Minute() >= t.Minute())
}

func scheduledStopBlocksAutomaticStart(account app.Account) bool {
	return account.ScheduleEnabled && account.ScheduleStopEnabled && account.ScheduleStopActive
}

// canKeepAlive is intentionally independent of instance billing type. An
// externally stopped instance is recoverable unless a local safety rule says
// that the stop was intentional or traffic protection is active.
func canKeepAlive(account app.Account, requiresProtection bool) bool {
	return !requiresProtection && !account.ScheduleBlockedByTraffic && !account.AutoStartBlocked && !scheduledStopBlocksAutomaticStart(account)
}

func sameMonth(unix int64, now time.Time) bool {
	if unix <= 0 {
		return false
	}
	t := time.Unix(unix, 0)
	return t.Year() == now.Year() && t.Month() == now.Month()
}

func (w *Worker) Run(ctx context.Context) {
	if w.Log == nil {
		w.Log = log.Default()
	}
	w.setRuntimeWaiting(runtimeJobQueue, "后台任务队列空闲", time.Time{})
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		job, err := w.Store.ClaimJob(2 * time.Minute)
		if err != nil {
			w.Log.Printf("claim job: %v", err)
			started := w.beginRuntime(runtimeJobQueue, "正在读取后台任务", time.Time{})
			w.finishRuntime(runtimeJobQueue, started, "后台任务读取失败", time.Time{}, err)
			sleep(ctx, 2*time.Second)
			continue
		}
		if job == nil {
			sleep(ctx, time.Second)
			continue
		}
		started := w.beginRuntime(runtimeJobQueue, "正在执行 "+job.Kind, time.Time{})
		if err := w.execute(ctx, job); err != nil {
			w.Log.Printf("job %s: %v", job.JobID, err)
			maxAttempts := 5
			if job.Kind == "create_ecs" {
				message := strings.ToLower(err.Error())
				for _, code := range []string{"quotaexceeded.vpc", "idempotentparametermismatch", "invalidaccountstatus.notenoughbalance", "invalidcidrblock.overlapped", "invalidparameter.instancetypenotsupport", "insufficientstock", "not_enough_stock"} {
					if strings.Contains(message, code) {
						maxAttempts = 1
						break
					}
				}
			}
			if job.Kind == "delete_instance" {
				maxAttempts = 20
			}
			if job.Attempts < maxAttempts {
				_ = w.Store.RetryJob(job.JobID, retryDelay(job.Attempts), err.Error())
				w.finishRuntime(runtimeJobQueue, started, "任务执行失败，已进入重试", time.Time{}, err)
			} else {
				if job.Kind == "delete_instance" {
					if id, parseErr := strconv.ParseInt(job.EntityKey, 10, 64); parseErr == nil {
						_ = w.Store.MarkReleaseFailed(id)
					}
				}
				_ = w.Store.FailJob(job.JobID, err.Error())
				w.Store.AddLog("error", fmt.Sprintf("后台任务最终失败 [%s/%s]（已重试 %d 次）：%s", job.Kind, job.EntityKey, job.Attempts, err.Error()))
				w.finishRuntime(runtimeJobQueue, started, "任务已达到最大重试次数", time.Time{}, err)
				action, entityType, summary := "job_execute", "job", "后台任务执行失败"
				if job.Kind == "create_ecs" {
					action, entityType, summary = "ecs_create", "ecs_task", "ECS 创建任务失败"
				} else if job.Kind == "delete_instance" {
					action, entityType, summary = "instance_release", "instance", "实例释放任务失败"
				}
				w.audit(app.AuditSourceJob, action, entityType, job.EntityKey, summary, err)
			}
			continue
		}
		_ = w.Store.FinishJob(job.JobID)
		action, entityType, summary := "job_execute", "job", "后台任务执行完成"
		if job.Kind == "create_ecs" {
			action, entityType, summary = "ecs_create", "ecs_task", "ECS 创建任务完成"
		} else if job.Kind == "delete_instance" {
			action, entityType, summary = "instance_release", "instance", "实例释放任务完成"
		}
		w.audit(app.AuditSourceJob, action, entityType, job.EntityKey, summary, nil)
		w.finishRuntime(runtimeJobQueue, started, "已完成 "+job.Kind, time.Time{}, nil)
		w.setRuntimeWaiting(runtimeJobQueue, "后台任务队列空闲", time.Time{})
	}
}

func (w *Worker) execute(ctx context.Context, job *store.Job) error {
	switch job.Kind {
	case "create_ecs":
		return w.createECS(ctx, job)
	case "delete_instance":
		return w.deleteInstance(ctx, job)
	default:
		return fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

func (w *Worker) deleteInstance(ctx context.Context, job *store.Job) error {
	id, err := strconv.ParseInt(job.EntityKey, 10, 64)
	if err != nil {
		return err
	}
	account, err := w.Store.Account(id, true)
	if err != nil {
		return err
	}
	client := w.cloudClient(app.AccountGroup{AccessKeyID: account.AccessKeyID, AccessKeySecret: account.AccessKeySecret, RegionID: account.RegionID, SiteType: account.SiteType})
	if client == nil {
		return fmt.Errorf("cloud client is not configured")
	}
	status := account.InstanceStatus
	notFound := false
	if status != "Stopped" && status != "Released" {
		instance, describeErr := client.DescribeInstance(ctx, account.RegionID, account.InstanceID)
		if cloud.IsNotFound(describeErr) {
			notFound = true
		} else if describeErr != nil {
			return describeErr
		} else if instance == nil {
			return fmt.Errorf("云端未返回实例状态，暂缓释放")
		} else {
			status = instance.Status
		}
	}
	if !notFound && status != "Stopped" && status != "Released" {
		if status != "Stopping" {
			if err := client.StopInstance(ctx, account.RegionID, account.InstanceID, app.ResolveShutdownMode(account.StoppedMode, w.Store.GetSetting("shutdown_mode", "KeepCharging"))); err != nil {
				return err
			}
			_ = w.Store.SetInstanceStatus(id, "Stopping")
		}
		return fmt.Errorf("实例当前状态为 %s，等待停机后继续释放", status)
	}
	if account.EIPManaged && account.EIPAllocationID != "" {
		if err := client.UnassociateEIP(ctx, account.RegionID, account.EIPAllocationID); err != nil && !cloud.IsNotFound(err) {
			return err
		}
		if err := client.ReleaseEIP(ctx, account.RegionID, account.EIPAllocationID); err != nil && !cloud.IsNotFound(err) {
			return err
		}
	}
	if !notFound {
		if err := client.DeleteInstance(ctx, account.RegionID, account.InstanceID); err != nil && !cloud.IsNotFound(err) {
			return err
		}
	}
	if err := w.Store.RemoveRotationMember(id); err != nil {
		return fmt.Errorf("移除实例分组关系失败: %w", err)
	}
	if err := w.Store.PhysicallyDelete(id); err != nil {
		return err
	}
	w.dispatchEvent(ctx, notify.Event{Title: "实例已释放", Summary: "实例已从云端释放，本地记录已清理。", AccountID: accountLabel(*account), Text: fmt.Sprintf("【ECS 控制台】ECS 已释放\n实例: %s\n实例 ID: %s\n区域: %s\n公网 IP: %s\n时间: %s", accountLabel(*account), account.InstanceID, account.RegionID, account.PublicIP, time.Now().Format("2006-01-02 15:04:05")), Fields: map[string]string{"instance_id": account.InstanceID, "region": account.RegionID, "public_ip": account.PublicIP}})
	w.Store.AddLog("info", "实例已异步释放: "+account.InstanceID)
	return nil
}

func (w *Worker) createECS(ctx context.Context, job *store.Job) (err error) {
	if w.CloudFactory == nil {
		return fmt.Errorf("cloud client is not configured")
	}
	task, err := w.Store.GetTaskForWorker(job.EntityKey)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return err
	}
	groups, err := w.Store.LoadGroups()
	if err != nil {
		return err
	}
	var group *app.AccountGroup
	for i := range groups {
		if groups[i].GroupKey == task.GroupKey {
			group = &groups[i]
			break
		}
	}
	if group == nil {
		return fmt.Errorf("account group %s not found", task.GroupKey)
	}
	client := w.cloudClient(*group)
	if client == nil {
		return fmt.Errorf("cloud client is not configured")
	}
	zoneID := stringOr(payload, "zoneId", "")
	if zoneID == "" || zoneID == "待由云 API 选择" {
		return fmt.Errorf("创建任务未指定可用区，请重新预览并选择可用区")
	}

	var createdInstance string
	var createdVPC, createdVSwitch, createdSecurityGroup string
	networkCreated := false
	defer func() {
		if err == nil {
			return
		}
		if createdInstance != "" {
			_ = w.Store.UpdateTask(task.TaskID, map[string]any{
				"status":         "partial",
				"step":           "实例已创建，后续配置失败",
				"error_message":  err.Error(),
				"instance_id":    createdInstance,
				"login_user":     stringOr(payload, "loginUser", "root"),
				"login_password": stringOr(payload, "loginPassword", task.LoginPassword),
			})
			w.Store.AddLog("warning", "ECS 已创建但后续配置失败，实例已保留: "+createdInstance+" / "+err.Error())
			err = nil
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if networkCreated {
			_ = client.CleanupNetwork(rollbackCtx, task.RegionID, createdVPC, createdVSwitch, createdSecurityGroup)
		}
		_ = w.Store.UpdateTask(task.TaskID, map[string]any{"status": "failed", "step": "创建失败", "error_message": err.Error()})
	}()

	_ = w.Store.UpdateTask(task.TaskID, map[string]any{"status": "running", "step": "准备网络"})
	vpcID := stringOr(payload, "vpcId", "")
	vswitchID := stringOr(payload, "vswitchId", "")
	securityGroupID := stringOr(payload, "securityGroupId", "")
	if vpcID == "" || vswitchID == "" || securityGroupID == "" {
		loginPort := intField(payload, "loginPort")
		osKey := stringOr(payload, "osKey", "")
		if loginPort == 0 {
			loginPort = loginPortForOS(osKey)
		}
		securityGroupPort := loginPort
		if !strings.HasPrefix(strings.ToLower(osKey), "windows") {
			securityGroupPort = cloud.AllInboundPorts
		}
		if reusableClient, ok := client.(cloud.ReusableNetworkClient); ok {
			prepared, prepareErr := reusableClient.PrepareReusableNetworkForPort(ctx, task.RegionID, stringOr(payload, "cidr", "192.168.0.0/16"), zoneID, stringOr(payload, "clientCidrIp", "127.0.0.1/32"), securityGroupPort)
			vpcID, vswitchID, securityGroupID, err = prepared.VPCID, prepared.VSwitchID, prepared.SecurityGroupID, prepareErr
			if prepared.CreatedVPC {
				createdVPC = prepared.VPCID
			}
			if prepared.CreatedVSwitch {
				createdVSwitch = prepared.VSwitchID
			}
			if prepared.CreatedSG {
				createdSecurityGroup = prepared.SecurityGroupID
			}
		} else if networkClient, ok := client.(cloud.NetworkClient); ok {
			vpcID, vswitchID, securityGroupID, err = networkClient.PrepareNetworkForPort(ctx, task.RegionID, stringOr(payload, "cidr", "192.168.0.0/16"), zoneID, stringOr(payload, "clientCidrIp", "127.0.0.1/32"), securityGroupPort)
			createdVPC, createdVSwitch, createdSecurityGroup = vpcID, vswitchID, securityGroupID
		} else {
			vpcID, vswitchID, securityGroupID, err = client.PrepareNetwork(ctx, task.RegionID, stringOr(payload, "cidr", "192.168.0.0/16"), zoneID, stringOr(payload, "clientCidrIp", "127.0.0.1/32"))
			createdVPC, createdVSwitch, createdSecurityGroup = vpcID, vswitchID, securityGroupID
		}
		networkCreated = createdVPC != "" || createdVSwitch != "" || createdSecurityGroup != ""
		if err != nil {
			return err
		}
		_ = w.Store.UpdateTask(task.TaskID, map[string]any{"vpc_id": vpcID, "vswitch_id": vswitchID, "security_group_id": securityGroupID})
	}

	_ = w.Store.UpdateTask(task.TaskID, map[string]any{"status": "running", "step": "创建 ECS 实例"})
	password := stringOr(payload, "loginPassword", task.LoginPassword)
	if stringOr(payload, "imageId", "") == "" {
		return fmt.Errorf("预检未返回有效镜像 ID")
	}
	run, err := client.RunInstances(ctx, cloud.RunRequest{RegionID: task.RegionID, ZoneID: zoneID, InstanceType: task.InstanceType, ImageID: stringOr(payload, "imageId", ""), InstanceName: stringOr(payload, "instanceName", "ecs-controller"), VPCID: vpcID, VSwitchID: vswitchID, SecurityGroupID: securityGroupID, Bandwidth: intField(payload, "internetMaxBandwidthOut"), DiskSize: intField(payload, "systemDiskSize"), DiskCategory: stringOr(payload, "systemDiskCategory", "cloud_essd"), PublicIPMode: stringOr(payload, "publicIpMode", "ecs_public_ip"), BillingMode: stringOr(payload, "billingMode", cloud.BillingModePostpaid), Password: password, LoginPort: intField(payload, "loginPort"), ClientToken: task.TaskID, UserData: stringOr(payload, "cloudInitUserData", "")})
	if err != nil {
		return err
	}
	createdInstance = run.InstanceID
	if createdInstance == "" {
		return fmt.Errorf("阿里云未返回实例 ID；抢占式库存可能已售罄，请更换可用区、实例规格或改用普通按量后重试")
	}
	_ = w.Store.UpdateTask(task.TaskID, map[string]any{"instance_id": createdInstance, "public_ip": run.PublicIP, "login_user": stringOr(payload, "loginUser", "root"), "login_password": password, "step": "等待实例网络就绪"})

	publicIP := run.PublicIP

	scheduleEnabled := boolField(payload, "scheduleEnabled")
	a := app.Account{AccessKeyID: group.AccessKeyID, AccessKeySecret: group.AccessKeySecret, RegionID: group.RegionID, InstanceID: createdInstance, MaxTraffic: group.MaxTraffic, ScheduleEnabled: scheduleEnabled, ScheduleStartEnabled: scheduleEnabled, ScheduleStopEnabled: scheduleEnabled, StartTime: stringOr(payload, "startTime", ""), StopTime: stringOr(payload, "stopTime", ""), StoppedMode: app.ResolveShutdownMode(stringOr(payload, "stoppedMode", ""), w.Store.GetSetting("shutdown_mode", "KeepCharging")), Remark: group.Remark, SiteType: group.SiteType, GroupKey: group.GroupKey, InstanceName: stringOr(payload, "instanceName", ""), InstanceType: task.InstanceType, InternetBandwidth: intField(payload, "internetMaxBandwidthOut"), PublicIP: publicIP, PublicIPMode: stringOr(payload, "publicIpMode", "ecs_public_ip"), InstanceStatus: "Running", UpdatedAt: time.Now().Unix()}
	if err = w.Store.UpsertAccount(a); err != nil {
		return err
	}
	_ = w.Store.UpdateTask(task.TaskID, map[string]any{"status": "success", "step": "创建完成", "public_ip": publicIP, "login_user": stringOr(payload, "loginUser", "root"), "login_password": password, "error_message": "", "public_ip_mode": stringOr(payload, "publicIpMode", "ecs_public_ip")})
	w.dispatchEvent(ctx, notify.Event{Title: "ECS 创建成功", Summary: "实例已创建并启动，请保存一次性登录密码。", AccountID: group.Remark, Text: fmt.Sprintf("【ECS 控制台】ECS 创建成功\n账号: %s\n实例 ID: %s\n区域: %s\n规格: %s\n公网 IP: %s\n登录用户: %s\n初始密码: %s\n请立即保存并修改初始密码。", group.Remark, createdInstance, task.RegionID, task.InstanceType, publicIP, stringOr(payload, "loginUser", "root"), password), Fields: map[string]string{"instance_id": createdInstance, "region": task.RegionID, "public_ip": publicIP, "password": password}})
	return nil
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(1<<min(attempt, 5)) * time.Second
	return d
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
func stringOr(m map[string]any, key, fallback string) string {
	if v, ok := stringField(m, key); ok && v != "" {
		return v
	}
	return fallback
}
func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}

func boolField(m map[string]any, key string) bool {
	switch value := m[key].(type) {
	case bool:
		return value
	case float64:
		return value != 0
	case int:
		return value != 0
	case string:
		return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
	default:
		return false
	}
}

func loginPortForOS(osKey string) int {
	if strings.HasPrefix(strings.ToLower(osKey), "windows") {
		return 3389
	}
	return 22
}
