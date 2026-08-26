package worker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

func (w *Worker) runBillingAlerts(ctx context.Context, now time.Time) {
	if w.Store.GetSetting("enable_billing", "0") != "1" {
		return
	}
	costThreshold, _ := strconv.ParseFloat(w.Store.GetSetting("billing_cost_threshold", "0"), 64)
	balanceThreshold, _ := strconv.ParseFloat(w.Store.GetSetting("billing_balance_threshold", "0"), 64)
	if costThreshold <= 0 && balanceThreshold <= 0 {
		return
	}
	hour := now.Format("2006-01-02-15")
	if w.Store.GetSetting("billing_alert_last_check_hour", "") == hour {
		return
	}
	_ = w.Store.SetSetting("billing_alert_last_check_hour", hour)
	groups, err := w.Store.LoadGroups()
	if err != nil {
		w.Store.AddLog("warning", "费用告警账号读取失败: "+err.Error())
		return
	}
	seen := make(map[string]struct{})
	cycle := now.Format("2006-01")
	for _, group := range groups {
		identity := strings.ToLower(strings.TrimSpace(group.SiteType)) + "\x00" + strings.TrimSpace(group.AccessKeyID)
		if strings.TrimSpace(group.AccessKeyID) == "" {
			continue
		}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		client := w.cloudClient(group)
		billingClient, ok := client.(cloud.BillingClient)
		if client == nil || !ok {
			continue
		}
		fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))[:16]
		label := groupLabel(group)
		if costThreshold > 0 && w.Store.GetSetting("billing_cost_alert_"+fingerprint, "") != cycle {
			if cost, currency, queryErr := billingClient.GetBillOverview(ctx, group.SiteType, cycle); queryErr == nil && cost >= costThreshold {
				text := fmt.Sprintf("【ECS 控制台】本月费用达到告警阈值\n账号: %s\n本月累计: %s\n告警阈值: %s\n说明: 仅发送通知，不会自动停机。", label, formatDailyBillingAmount(cost, currency), formatDailyBillingAmount(costThreshold, currency))
				if dispatchErr := w.dispatchEvent(ctx, notify.Event{Title: "费用阈值告警", Summary: fmt.Sprintf("%s 本月费用已达到阈值", label), AccountID: label, Text: text, Fields: map[string]string{"billing_cycle": cycle, "action": "notify_only"}}); dispatchErr == nil {
					_ = w.Store.SetSetting("billing_cost_alert_"+fingerprint, cycle)
					w.audit(app.AuditSourceSchedule, "billing_cost_alert", "account", fingerprint, "本月费用达到阈值，仅发送通知", nil)
				}
			}
		}
		day := now.Format("2006-01-02")
		if balanceThreshold > 0 && w.Store.GetSetting("billing_balance_alert_"+fingerprint, "") != day {
			if balance, currency, queryErr := billingClient.GetAccountBalance(ctx, group.SiteType); queryErr == nil && balance <= balanceThreshold {
				text := fmt.Sprintf("【ECS 控制台】账户余额达到告警阈值\n账号: %s\n当前余额: %s\n告警阈值: %s\n说明: 仅发送通知，不会自动停机。", label, formatDailyBillingAmount(balance, currency), formatDailyBillingAmount(balanceThreshold, currency))
				if dispatchErr := w.dispatchEvent(ctx, notify.Event{Title: "余额阈值告警", Summary: fmt.Sprintf("%s 账户余额已达到阈值", label), AccountID: label, Text: text, Fields: map[string]string{"alert_date": day, "action": "notify_only"}}); dispatchErr == nil {
					_ = w.Store.SetSetting("billing_balance_alert_"+fingerprint, day)
					w.audit(app.AuditSourceSchedule, "billing_balance_alert", "account", fingerprint, "账户余额达到阈值，仅发送通知", nil)
				}
			}
		}
	}
}
