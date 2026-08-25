package worker

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

const (
	dailyTrafficEnabledSetting = "notify_daily_enabled"
	dailyTrafficTimeSetting    = "notify_daily_time"
	dailyTrafficLastDate       = "notify_daily_last_date"
	dailyBillingLastDate       = "notify_daily_billing_last_date"
)

func (w *Worker) runDailyTrafficSummary(ctx context.Context, now time.Time) {
	if w.Store.GetSetting(dailyTrafficEnabledSetting, "0") != "1" {
		return
	}
	reportTime, err := time.ParseInLocation("15:04", w.Store.GetSetting(dailyTrafficTimeSetting, "00:00"), now.Location())
	if err != nil {
		w.Store.AddLog("warning", "每日流量摘要时间配置无效")
		return
	}
	scheduled := time.Date(now.Year(), now.Month(), now.Day(), reportTime.Hour(), reportTime.Minute(), 0, 0, now.Location())
	if now.Before(scheduled) {
		return
	}
	reportDay := now.AddDate(0, 0, -1)
	reportDate := reportDay.Format("2006-01-02")
	trafficPending := w.Store.GetSetting(dailyTrafficLastDate, "") != reportDate
	billingPending := w.Store.GetSetting("enable_billing", "0") == "1" && w.Store.GetSetting(dailyBillingLastDate, "") != reportDate
	if !trafficPending && !billingPending {
		return
	}

	settings := w.Store.Settings()
	cfg := notify.ConfigFromSettings(settings, w.Store.OpenSecret)
	if !hasConfiguredNotificationChannel(cfg) {
		return
	}
	if trafficPending {
		event, err := w.dailyTrafficEvent(ctx, reportDay)
		if err != nil {
			w.Store.AddLog("warning", "每日流量摘要生成失败: "+err.Error())
		} else if err := w.dispatchEvent(ctx, event); err == nil {
			if err := w.Store.SetSetting(dailyTrafficLastDate, reportDate); err != nil {
				w.Store.AddLog("warning", "每日流量摘要发送状态保存失败: "+err.Error())
			}
		}
	}

	if billingPending {
		event, err := w.dailyBillingEvent(ctx, reportDay)
		if err != nil {
			w.Store.AddLog("warning", "每日消费情况生成失败: "+err.Error())
		} else if err := w.dispatchEvent(ctx, event); err == nil {
			if err := w.Store.SetSetting(dailyBillingLastDate, reportDate); err != nil {
				w.Store.AddLog("warning", "每日消费情况发送状态保存失败: "+err.Error())
			}
		}
	}
}

func hasConfiguredNotificationChannel(cfg notify.EventConfig) bool {
	return (cfg.EmailEnabled && cfg.Email != "" && cfg.SMTPHost != "") ||
		(cfg.TelegramEnabled && cfg.TelegramToken != "" && cfg.TelegramChatID != "") ||
		(cfg.WebhookEnabled && cfg.WebhookURL != "")
}

func (w *Worker) dailyTrafficEvent(ctx context.Context, reportDay time.Time) (notify.Event, error) {
	accounts, err := w.Store.LoadAccounts(false)
	if err != nil {
		return notify.Event{}, err
	}
	groups, err := w.Store.LoadGroups()
	if err != nil {
		return notify.Event{}, err
	}

	complete := true
	cmsLines := make([]string, 0)
	dayStart := time.Date(reportDay.Year(), reportDay.Month(), reportDay.Day(), 0, 0, 0, 0, reportDay.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	for _, account := range accounts {
		if account.InstanceID == "" || account.TrafficAPIStatus == "fallback_cdt" {
			continue
		}
		if account.TrafficAPIStatus == "error" {
			complete = false
			cmsLines = append(cmsLines, fmt.Sprintf("- %s：数据不可用", accountLabel(account)))
			continue
		}
		client := w.cloudClient(app.AccountGroup{
			AccessKeyID:     account.AccessKeyID,
			AccessKeySecret: account.AccessKeySecret,
			RegionID:        account.RegionID,
			SiteType:        account.SiteType,
		})
		if dailyClient, ok := client.(cloud.DailyTrafficClient); ok {
			bytes, points, err := dailyClient.GetInstanceDailyTraffic(
				ctx,
				account.RegionID,
				account.InstanceID,
				account.PublicIP,
				dayStart.UnixMilli(),
				dayEnd.UnixMilli(),
			)
			if err != nil {
				complete = false
				cmsLines = append(cmsLines, fmt.Sprintf("- %s：数据不可用", accountLabel(account)))
				continue
			}
			if points == 0 {
				complete = false
				cmsLines = append(cmsLines, fmt.Sprintf("- %s：数据不足", accountLabel(account)))
				continue
			}
			cmsLines = append(cmsLines, fmt.Sprintf("- %s：%.2f GB", accountLabel(account), bytes/(1024*1024*1024)))
			continue
		}

		traffic, ok, deltaErr := w.Store.DailyTrafficDelta(account.ID, reportDay)
		if deltaErr != nil {
			return notify.Event{}, deltaErr
		}
		if !ok {
			complete = false
			cmsLines = append(cmsLines, fmt.Sprintf("- %s：数据不足", accountLabel(account)))
			continue
		}
		cmsLines = append(cmsLines, fmt.Sprintf("- %s：%.2f GB", accountLabel(account), traffic))
	}
	if len(cmsLines) == 0 {
		cmsLines = append(cmsLines, "- 暂无有效 CMS 实例采样")
	}

	cdtLines := make([]string, 0, len(groups))
	for _, group := range groups {
		client := w.cloudClient(group)
		if client == nil {
			complete = false
			cdtLines = append(cdtLines, fmt.Sprintf("- %s：数据不可用", groupLabel(group)))
			continue
		}
		traffic, trafficErr := client.GetTraffic(ctx, group.RegionID)
		if trafficErr != nil {
			complete = false
			cdtLines = append(cdtLines, fmt.Sprintf("- %s：数据不可用", groupLabel(group)))
			continue
		}
		if group.MaxTraffic > 0 {
			cdtLines = append(cdtLines, fmt.Sprintf("- %s：%.2f GB/%g GB", groupLabel(group), traffic, group.MaxTraffic))
		} else {
			complete = false
			cdtLines = append(cdtLines, fmt.Sprintf("- %s：%.2f GB/额度未设置", groupLabel(group), traffic))
		}
	}
	if len(cdtLines) == 0 {
		cdtLines = append(cdtLines, "- 暂无有效 CDT 账号数据")
	}

	status := "完整"
	if !complete {
		status = "不完整"
	}
	text := strings.Join([]string{
		fmt.Sprintf("昨日流量摘要（%s）", reportDay.Format("2006-01-02")),
		"",
		"CMS 实例昨日消耗流量：",
		strings.Join(cmsLines, "\n"),
		"",
		"CDT 账号流量已使用：",
		strings.Join(cdtLines, "\n"),
		"",
		"数据状态：" + status,
	}, "\n")
	return notify.Event{
		Title:   "每日流量摘要",
		Summary: fmt.Sprintf("昨日流量摘要（%s）", reportDay.Format("2006-01-02")),
		Text:    text,
		Fields: map[string]string{
			"report_date": reportDay.Format("2006-01-02"),
			"data_status": status,
		},
	}, nil
}

func (w *Worker) dailyBillingEvent(ctx context.Context, reportDay time.Time) (notify.Event, error) {
	groups, err := w.Store.LoadGroups()
	if err != nil {
		return notify.Event{}, err
	}

	cycle := reportDay.Format("2006-01")
	complete := true
	lines := make([]string, 0, len(groups))
	seenAccounts := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		accountKey := strings.TrimSpace(group.SiteType) + "\x00" + strings.TrimSpace(group.AccessKeyID)
		if strings.TrimSpace(group.AccessKeyID) == "" {
			continue
		}
		if _, exists := seenAccounts[accountKey]; exists {
			continue
		}
		seenAccounts[accountKey] = struct{}{}

		client := w.cloudClient(group)
		billingClient, ok := client.(cloud.BillingClient)
		if client == nil || !ok {
			complete = false
			lines = append(lines, fmt.Sprintf("- %s：数据不可用", groupLabel(group)))
			continue
		}

		monthlyCost, costCurrency, costErr := billingClient.GetBillOverview(ctx, group.SiteType, cycle)
		balance, balanceCurrency, balanceErr := billingClient.GetAccountBalance(ctx, group.SiteType)
		parts := make([]string, 0, 2)
		if costErr != nil {
			complete = false
			parts = append(parts, "本月消费不可用")
		} else {
			parts = append(parts, "本月消费 "+formatDailyBillingAmount(monthlyCost, costCurrency))
		}
		if balanceErr != nil {
			complete = false
			parts = append(parts, "账户余额不可用")
		} else {
			parts = append(parts, "账户余额 "+formatDailyBillingAmount(balance, balanceCurrency))
		}
		lines = append(lines, fmt.Sprintf("- %s：%s", groupLabel(group), strings.Join(parts, "；")))
	}
	if len(lines) == 0 {
		complete = false
		lines = append(lines, "- 暂无可查询的云账号")
	}

	status := "完整"
	if !complete {
		status = "不完整"
	}
	text := strings.Join([]string{
		fmt.Sprintf("消费情况（%s）", cycle),
		"",
		strings.Join(lines, "\n"),
		"",
		"数据状态：" + status,
		"说明：云端账单可能存在延迟。",
	}, "\n")
	return notify.Event{
		Title:   "每日消费情况",
		Summary: fmt.Sprintf("%s 消费情况", cycle),
		Text:    text,
		Fields: map[string]string{
			"billing_cycle": cycle,
			"data_status":   status,
		},
	}, nil
}

func formatDailyBillingAmount(value float64, currency string) string {
	precision := 2
	if value != 0 && math.Abs(value) < 0.01 {
		precision = 6
	}
	amount := strconv.FormatFloat(value, 'f', precision, 64)
	if precision > 2 {
		amount = strings.TrimRight(strings.TrimRight(amount, "0"), ".")
	} else {
		amount = strings.TrimSuffix(amount, ".00")
	}
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "USD":
		return "$" + amount
	case "CNY", "":
		return "¥" + amount
	default:
		return strings.ToUpper(strings.TrimSpace(currency)) + " " + amount
	}
}

func groupLabel(group app.AccountGroup) string {
	if strings.TrimSpace(group.Remark) != "" {
		return group.Remark
	}
	if strings.TrimSpace(group.AccessKeyID) != "" {
		return group.AccessKeyID
	}
	return group.RegionID
}
