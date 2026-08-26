package worker

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

func (w *Worker) runRotationSchedule(ctx context.Context, client cloud.Client, account *app.Account, now time.Time, shutdownMode string) bool {
	shutdownMode = app.ResolveShutdownMode(account.StoppedMode, shutdownMode)
	group, member := w.rotationGroupForAccount(account.ID)
	if group == nil || member == nil {
		return false
	}
	if !account.ScheduleEnabled || !account.ScheduleStartEnabled || !account.ScheduleStopEnabled {
		return true
	}
	states, err := w.Store.RotationStates(group.ID)
	if err != nil {
		w.Store.AddLog("error", "读取分组轮转状态失败: "+err.Error())
		return true
	}
	state := states[account.ID]
	state.GroupID, state.AccountID = group.ID, account.ID

	startDate, startAt, startExists := rotationOccurrence(now, account.StartTime, -group.AdvanceStartMinutes)
	stopDate, stopAt, stopExists := rotationOccurrence(now, account.StopTime, group.DelayStopMinutes)
	activeWindow := startExists && (!stopExists || startAt.After(stopAt))
	startDue := startExists && startDate != state.LastStartDate
	stopDue := stopExists && stopDate != state.LastStopDate
	if startDue && activeWindow {
		switch account.InstanceStatus {
		case "Stopped":
			if err := client.StartInstance(ctx, account.RegionID, account.InstanceID); err != nil {
				w.rotationAlert(ctx, group, account, &state, "分组提前开机失败: "+err.Error())
				w.audit(app.AuditSourceRotation, "rotation_advance_start", "instance", auditAccountID(*account), "分组提前开机", err)
			} else {
				account.InstanceStatus = "Starting"
				account.ScheduleStopActive = false
				state.LastStartDate = startDate
				state.LastError = ""
				w.Store.AddLog("info", fmt.Sprintf("分组 [%s] 提前启动实例: %s", group.Name, account.InstanceID))
				w.audit(app.AuditSourceRotation, "rotation_advance_start", "instance", auditAccountID(*account), "分组提前开机", nil)
			}
		case "Running", "Starting", "Pending":
			state.LastStartDate = startDate
		}
	} else if startDue {
		// The effective stop time is newer than the missed start time. Record the
		// old start occurrence without starting an instance whose window ended.
		state.LastStartDate = startDate
	}

	if activeWindow && account.InstanceStatus == "Running" && state.LastStartDate != "" && state.DNSUpdatedDate != state.LastStartDate {
		address := account.PublicIP
		if account.PublicIPMode == "eip" && account.EIPAddress != "" {
			address = account.EIPAddress
		}
		if address == "" {
			dnsErr := fmt.Errorf("实例没有公网 IP")
			w.rotationAlert(ctx, group, account, &state, "分组 DDNS 更新失败: "+dnsErr.Error())
			w.audit(app.AuditSourceRotation, "ddns_update", "rotation_group", group.ID, "更新分组域名解析", dnsErr)
		} else if err := notify.UpdateDNSRecord(ctx, rotationDNSConfig(*group), address); err != nil {
			w.rotationAlert(ctx, group, account, &state, "分组 DDNS 更新失败: "+err.Error())
			w.audit(app.AuditSourceRotation, "ddns_update", "rotation_group", group.ID, "更新分组域名解析", err)
		} else {
			state.DNSUpdatedDate = state.LastStartDate
			state.LastError = ""
			w.Store.AddLog("info", fmt.Sprintf("分组 [%s] DDNS 已切换 %s -> %s", group.Name, group.Domain, address))
			w.audit(app.AuditSourceRotation, "ddns_update", "rotation_group", group.ID, "更新分组域名解析", nil)
		}
	}

	if stopDue && (!startExists || stopAt.After(startAt)) {
		switch account.InstanceStatus {
		case "Running":
			if err := client.StopInstance(ctx, account.RegionID, account.InstanceID, shutdownMode); err != nil {
				w.rotationAlert(ctx, group, account, &state, "分组延后关机失败: "+err.Error())
				w.audit(app.AuditSourceRotation, "rotation_delayed_stop", "instance", auditAccountID(*account), "分组延后关机", err)
			} else {
				account.InstanceStatus = "Stopping"
				account.ScheduleStopActive = true
				state.LastStopDate = stopDate
				w.Store.AddLog("info", fmt.Sprintf("分组 [%s] 延后关闭实例: %s", group.Name, account.InstanceID))
				w.dispatchEvent(ctx, statusEvent(*account, "Running", "Stopping", fmt.Sprintf("分组 %s 已到延后关机时间。", group.Name)))
				w.audit(app.AuditSourceRotation, "rotation_delayed_stop", "instance", auditAccountID(*account), "分组延后关机", nil)
			}
		case "Stopped", "Stopping":
			state.LastStopDate = stopDate
			account.ScheduleStopActive = true
		}
	} else if stopDue {
		// The effective start time is newer than the missed stop time. Do not
		// immediately shut down an instance that has just entered its next window.
		state.LastStopDate = stopDate
	}
	if err := w.Store.SaveRotationState(state); err != nil {
		w.Store.AddLog("error", "保存分组轮转状态失败: "+err.Error())
	}
	return true
}

func (w *Worker) rotationGroupForAccount(accountID int64) (*app.RotationGroup, *app.RotationMember) {
	groups, err := w.Store.LoadRotationGroups()
	if err != nil {
		return nil, nil
	}
	for i := range groups {
		for j := range groups[i].Members {
			if groups[i].Members[j].AccountID == accountID {
				return &groups[i], &groups[i].Members[j]
			}
		}
	}
	return nil, nil
}

func (w *Worker) rotationManaged(accountID int64) bool {
	group, member := w.rotationGroupForAccount(accountID)
	return group != nil && member != nil
}

func rotationDue(now time.Time, configured string, offsetMinutes int, lastDate string) (string, bool) {
	date, _, ok := rotationOccurrence(now, configured, offsetMinutes)
	return date, ok && date != lastDate
}

func rotationOccurrence(now time.Time, configured string, offsetMinutes int) (string, time.Time, bool) {
	clock, err := time.ParseInLocation("15:04", configured, now.Location())
	if err != nil {
		return "", time.Time{}, false
	}
	type occurrence struct {
		date string
		at   time.Time
	}
	items := make([]occurrence, 0, 3)
	for dayOffset := -1; dayOffset <= 1; dayOffset++ {
		day := now.AddDate(0, 0, dayOffset)
		base := time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
		actual := base.Add(time.Duration(offsetMinutes) * time.Minute)
		date := base.Format("2006-01-02")
		if !actual.After(now) && now.Sub(actual) < 26*time.Hour {
			items = append(items, occurrence{date: date, at: actual})
		}
	}
	if len(items) == 0 {
		return "", time.Time{}, false
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	return items[0].date, items[0].at, true
}

func rotationDNSConfig(group app.RotationGroup) notify.DNSUpdateConfig {
	return notify.DNSUpdateConfig{
		Provider: group.Provider, Domain: group.Domain, TTL: group.DNS.TTL,
		CloudflareToken: group.DNS.CloudflareToken, CloudflareProxied: group.DNS.CloudflareProxied,
		DNSPodSecretID: group.DNS.DNSPodSecretID, DNSPodSecretKey: group.DNS.DNSPodSecretKey,
		AliDNSAccessKeyID: group.DNS.AliDNSAccessKeyID, AliDNSAccessKeySecret: group.DNS.AliDNSSecret,
	}
}

func (w *Worker) rotationAlert(ctx context.Context, group *app.RotationGroup, account *app.Account, state *app.RotationState, message string) {
	w.Store.AddLog("warning", message)
	if state.LastError == message {
		return
	}
	state.LastError = message
	w.dispatchEvent(ctx, notify.Event{Title: "分组轮转告警", Summary: message, AccountID: accountLabel(*account), Text: fmt.Sprintf("【ECS 控制台】分组轮转告警\n分组: %s\n共享域名: %s\n实例: %s\n原因: %s", group.Name, group.Domain, account.InstanceID, message), Fields: map[string]string{"group": group.Name, "domain": group.Domain, "instance_id": account.InstanceID}})
}
