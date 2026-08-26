package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

// TelegramControl polls only while the feature is configured. An unconfigured
// installation sleeps between checks, so the optional worker cannot spin at
// 100% CPU before the first setup.
func (w *Worker) TelegramControl(ctx context.Context) {
	for {
		if !w.telegramConfigured() {
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		client, err := w.telegramClient()
		if err != nil {
			w.Store.AddLog("error", "Telegram 控制客户端初始化失败: "+err.Error())
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		offset := w.telegramOffset(w.telegramToken())
		updates, err := client.GetUpdates(ctx, offset+1)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.Store.AddLog("error", "Telegram 控制拉取消息失败: "+err.Error())
			if !sleepContext(ctx, 5*time.Second) {
				return
			}
			continue
		}
		_ = w.Store.CleanupTelegramActionTokens()
		for _, update := range updates {
			if id := int64(numberValue(update["update_id"])); id > 0 {
				// Advance before handling so a restart cannot repeat a destructive
				// callback after the Bot API has delivered it once.
				_ = w.Store.SetTelegramState("last_update_id", strconv.FormatInt(id, 10))
			}
			if err := w.handleTelegramUpdate(ctx, client, update); err != nil {
				w.Store.AddLog("error", "Telegram 控制指令处理失败: "+err.Error())
			}
		}
	}
}

func (w *Worker) telegramOffset(token string) int64 {
	fingerprint := telegramTokenFingerprint(token)
	if w.Store.GetTelegramState("token_fingerprint", "") != fingerprint {
		// Update IDs are scoped to a bot. Never reuse an offset from a previous
		// token, otherwise a newly configured bot can skip all incoming messages.
		_ = w.Store.SetTelegramState("token_fingerprint", fingerprint)
		_ = w.Store.SetTelegramState("last_update_id", "0")
		return 0
	}
	offset, _ := strconv.ParseInt(w.Store.GetTelegramState("last_update_id", "0"), 10, 64)
	return offset
}

func telegramTokenFingerprint(token string) string {
	// Include the parser version so upgrades that change update handling get a
	// clean polling offset once instead of silently losing new messages.
	sum := sha256.Sum256([]byte("v2:" + strings.TrimSpace(token)))
	return fmt.Sprintf("%x", sum[:])
}

func (w *Worker) telegramConfigured() bool {
	settings := w.Store.Settings()
	return telegramSettingEnabled(settings["notify_tg_enabled"]) && strings.TrimSpace(settings["notify_tg_chat_id"]) != "" && strings.TrimSpace(w.telegramToken()) != ""
}

func telegramSettingEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (w *Worker) telegramToken() string {
	value, _ := w.Store.OpenSecret(w.Store.GetSetting("notify_tg_token", ""))
	return value
}

func (w *Worker) telegramClient() (*notify.TelegramClient, error) {
	settings := w.Store.Settings()
	proxyPass, _ := w.Store.OpenSecret(settings["notify_tg_proxy_pass"])
	return notify.NewTelegramClient(w.telegramToken(), settings["notify_tg_proxy_type"], settings["notify_tg_proxy_url"], settings["notify_tg_proxy_ip"], settings["notify_tg_proxy_port"], settings["notify_tg_proxy_user"], proxyPass)
}

func (w *Worker) handleTelegramUpdate(ctx context.Context, client *notify.TelegramClient, update map[string]any) error {
	if callback, ok := update["callback_query"].(map[string]any); ok {
		return w.handleTelegramCallback(ctx, client, callback)
	}
	if message, ok := update["message"].(map[string]any); ok {
		return w.handleTelegramMessage(ctx, client, message)
	}
	return nil
}

func (w *Worker) handleTelegramMessage(ctx context.Context, client *notify.TelegramClient, message map[string]any) error {
	chatID := stringValue(messageMap(message, "chat", "id"))
	userID := stringValue(messageMap(message, "from", "id"))
	if !w.telegramAllowed(chatID, userID) {
		return nil
	}
	text := strings.TrimSpace(stringValue(message["text"]))
	command := ""
	if text == "" {
		command = ""
	} else {
		command = strings.ToLower(strings.TrimSpace(strings.Split(strings.Fields(text)[0], "@")[0]))
	}
	var body string
	var keyboard any
	switch command {
	case "/traffic", "流量":
		body, keyboard = w.telegramTraffic(ctx), trafficKeyboard()
	case "/instances", "实例":
		body, keyboard = w.telegramInstances(1), w.instancesKeyboard(1)
	default:
		body, keyboard = w.telegramHome(), w.mainKeyboard()
	}
	return client.SendMessage(ctx, chatID, body, keyboard)
}

func (w *Worker) handleTelegramCallback(ctx context.Context, client *notify.TelegramClient, callback map[string]any) error {
	message, _ := callback["message"].(map[string]any)
	chatID := stringValue(messageMap(message, "chat", "id"))
	userID := stringValue(messageMap(callback, "from", "id"))
	callbackID := stringValue(callback["id"])
	if !w.telegramAllowed(chatID, userID) {
		return client.AnswerCallback(ctx, callbackID, "没有权限执行该操作")
	}
	data := strings.Split(stringValue(callback["data"]), ":")
	if len(data) < 2 || data[0] != "m" {
		return client.AnswerCallback(ctx, callbackID, "")
	}
	action := data[1]
	messageID := strconv.FormatInt(int64(numberValue(message["message_id"])), 10)
	answer := ""
	if action == "traffic" || action == "listrefresh" || action == "refreshall" || action == "refresh" || action == "start" || action == "stop" || action == "confirm" {
		answer = "正在处理..."
	}
	_ = client.AnswerCallback(ctx, callbackID, answer)

	edit := func(body string, keyboard any) error {
		if err := client.EditMessage(ctx, chatID, messageID, body, keyboard); err != nil {
			return client.SendMessage(ctx, chatID, body, keyboard)
		}
		return nil
	}
	switch action {
	case "home":
		return edit(w.telegramHome(), w.mainKeyboard())
	case "help":
		return edit("📘 使用说明\n\n从首页进入流量总览或实例管理。\n\n实例操作会先同步云端状态；释放实例需要二次确认，并交给后台队列回收 ECS、EIP 和 DDNS。", w.mainKeyboard())
	case "traffic":
		if err := w.refreshAllTelegramData(ctx); err != nil {
			w.Store.AddLog("warning", "Telegram 刷新流量失败: "+err.Error())
		}
		return edit(w.telegramTraffic(ctx), trafficKeyboard())
	case "refreshall":
		if err := w.refreshAllTelegramData(ctx); err != nil {
			w.Store.AddLog("warning", "Telegram 刷新数据失败: "+err.Error())
		}
		return edit(w.telegramHome(), w.mainKeyboard())
	case "list":
		return edit(w.telegramInstances(maxInt(1, intValueAt(data, 2))), w.instancesKeyboard(maxInt(1, intValueAt(data, 2))))
	case "listrefresh":
		if err := w.refreshAllTelegramData(ctx); err != nil {
			w.Store.AddLog("warning", "Telegram 刷新实例失败: "+err.Error())
		}
		page := maxInt(1, intValueAt(data, 2))
		return edit(w.telegramInstances(page), w.instancesKeyboard(page))
	case "inst":
		id := int64(intValueAt(data, 2))
		return edit(w.telegramInstance(id), w.instanceKeyboard(id, maxInt(1, intValueAt(data, 3))))
	case "refresh":
		id := int64(intValueAt(data, 2))
		page := maxInt(1, intValueAt(data, 3))
		if err := w.refreshTelegramAccount(ctx, id); err != nil {
			w.audit(app.AuditSourceTelegram, "instance_refresh", "instance", strconv.FormatInt(id, 10), "Telegram 刷新实例", err)
			return edit("❌ 刷新失败："+err.Error(), w.instanceKeyboard(id, page))
		}
		w.audit(app.AuditSourceTelegram, "instance_refresh", "instance", strconv.FormatInt(id, 10), "Telegram 刷新实例", nil)
		return edit(w.telegramInstance(id), w.instanceKeyboard(id, page))
	case "start", "stop":
		id := int64(intValueAt(data, 2))
		page := maxInt(1, intValueAt(data, 3))
		if err := w.controlTelegramAccount(ctx, id, action); err != nil {
			w.audit(app.AuditSourceTelegram, "instance_"+action, "instance", strconv.FormatInt(id, 10), "Telegram "+actionLabel(action), err)
			return edit("❌ "+actionLabel(action)+"失败："+err.Error(), w.instanceKeyboard(id, page))
		}
		w.audit(app.AuditSourceTelegram, "instance_"+action, "instance", strconv.FormatInt(id, 10), "Telegram "+actionLabel(action), nil)
		return edit("✅ "+actionLabel(action)+"指令已提交。", w.instanceKeyboard(id, page))
	case "release":
		id := int64(intValueAt(data, 2))
		page := maxInt(1, intValueAt(data, 3))
		if _, err := w.Store.Account(id, false); err != nil {
			return edit("❌ 实例不存在或已被清理。", w.mainKeyboard())
		}
		token := randomActionToken()
		ttl := w.telegramConfirmTTL()
		if err := w.Store.CreateTelegramActionToken(token, userID, chatID, "release", id, "", ttl); err != nil {
			return err
		}
		return edit(w.telegramReleaseConfirm(id, ttl), releaseKeyboard(token, id, page))
	case "confirm":
		if len(data) < 3 {
			return edit("⏱️ 释放确认已失效，请重新发起释放。", w.mainKeyboard())
		}
		record, err := w.Store.UseTelegramActionToken(data[2], userID, chatID)
		if err != nil {
			return err
		}
		if record == nil || record.Action != "release" {
			return edit("⏱️ 释放确认已失效，请重新发起释放。", w.mainKeyboard())
		}
		if err := w.enqueueTelegramDelete(record.AccountID); err != nil {
			w.audit(app.AuditSourceTelegram, "instance_release_submit", "instance", strconv.FormatInt(record.AccountID, 10), "Telegram 提交实例释放", err)
			return edit("❌ 释放指令提交失败："+err.Error(), w.mainKeyboard())
		}
		w.audit(app.AuditSourceTelegram, "instance_release_submit", "instance", strconv.FormatInt(record.AccountID, 10), "Telegram 提交实例释放", nil)
		return edit("🗑️ 释放指令已提交\n\n后台队列会继续回收 ECS、EIP 和 DDNS。", w.mainKeyboard())
	case "cancel":
		if len(data) >= 3 {
			_, _ = w.Store.UseTelegramActionToken(data[2], userID, chatID)
		}
		return edit("已取消释放操作。", w.mainKeyboard())
	default:
		return nil
	}
}

func (w *Worker) telegramAllowed(chatID, userID string) bool {
	configured := strings.TrimSpace(w.Store.GetSetting("notify_tg_chat_id", ""))
	if configured == "" || chatID != configured {
		return false
	}
	allowed := splitIDs(w.Store.GetSetting("notify_tg_allowed_user_ids", ""))
	if len(allowed) > 0 {
		for _, id := range allowed {
			if id == userID {
				return true
			}
		}
		return false
	}
	return chatID != "" && !strings.HasPrefix(chatID, "-") && chatID == userID
}

func (w *Worker) telegramHome() string {
	groups, _ := w.Store.LoadGroups()
	accounts, _ := w.Store.LoadAccounts(false)
	running, starting, stopped, other := 0, 0, 0, 0
	for _, account := range accounts {
		switch account.InstanceStatus {
		case "Running":
			running++
		case "Starting", "Stopping", "Releasing":
			starting++
		case "Stopped":
			stopped++
		default:
			other++
		}
	}

	lines := []string{
		"🛡️ ECS 控制台",
		"",
		fmt.Sprintf("资源总览  ·  %d 个账号  ·  %d 台实例", len(groups), len(accounts)),
		fmt.Sprintf("🟢 运行 %d    ⏸️ 停止 %d    🔄 处理中 %d", running, stopped, starting),
	}
	if other > 0 {
		lines = append(lines, fmt.Sprintf("⚪ 未知状态 %d", other))
	}
	lines = append(lines, "", "选择要进入的区域：")
	return strings.Join(lines, "\n")
}

func (w *Worker) telegramTraffic(ctx context.Context) string {
	groups, _ := w.Store.LoadGroups()
	accounts, _ := w.Store.LoadAccounts(false)
	instanceUsed, instanceAvailable := w.telegramInstanceTraffic(accounts, time.Now())
	cdtUsed := w.telegramCDTTraffic(ctx, groups, accounts, time.Now())
	if len(groups) == 0 {
		return "📊 账号概览\n\n暂无账号数据。"
	}
	threshold := numberValue(w.Store.GetSetting("traffic_threshold", "95"))
	lines := []string{"📊 账号概览"}
	for _, group := range groups {
		key := group.GroupKey
		if key == "" {
			key = group.AccessKeyID + "|" + group.RegionID
		}
		instanceTraffic := instanceUsed[key]
		cdtTraffic, hasCDT := cdtUsed[key]
		percentTraffic := instanceTraffic
		if hasCDT && cdtTraffic > percentTraffic {
			percentTraffic = cdtTraffic
		}
		percent := 0.0
		if group.MaxTraffic > 0 {
			percent = percentTraffic / group.MaxTraffic * 100
		}
		status := "正常"
		if percent >= 100 {
			status = "已超量"
		} else if percent >= threshold {
			status = "接近阈值"
		}
		cdtLine := "📡 CDT 流量：暂不可用"
		if hasCDT {
			cdtLine = fmt.Sprintf("📡 CDT 流量：%.2f GB / %.2f GB", cdtTraffic, group.MaxTraffic)
		}
		instanceLine := "🖥️ 实例流量：暂不可用"
		if instanceAvailable[key] {
			instanceLine = fmt.Sprintf("🖥️ 实例流量：%.2f GB / %.2f GB", instanceTraffic, group.MaxTraffic)
		}
		lines = append(lines, "", "👤 "+firstNonEmpty(group.Remark, "未命名账号"), "📍 "+group.RegionID, cdtLine, instanceLine, fmt.Sprintf("📈 使用率：%.0f%%（取两者较高值）", percent), statusIconForTraffic(status)+" "+status)
	}
	return strings.Join(lines, "\n")
}

func (w *Worker) telegramCDTTraffic(ctx context.Context, groups []app.AccountGroup, accounts []app.Account, now time.Time) map[string]float64 {
	result := make(map[string]float64, len(groups))
	cycle := now.Format("2006-01")
	for _, group := range groups {
		key := group.GroupKey
		var account *app.Account
		for i := range accounts {
			if accounts[i].GroupKey == key || (accounts[i].AccessKeyID == group.AccessKeyID && accounts[i].RegionID == group.RegionID) {
				account = &accounts[i]
				break
			}
		}
		if account == nil {
			continue
		}
		if cached, ok := w.Store.GetBillingCache(account.ID, "cdt_traffic", cycle, 5*time.Minute); ok {
			result[key] = numberValue(cached["traffic"])
			continue
		}
		client := w.clientForAccount(*account)
		if client == nil {
			continue
		}
		traffic, err := client.GetTraffic(ctx, group.RegionID)
		if err != nil {
			continue
		}
		result[key] = traffic
		_ = w.Store.SetBillingCache(account.ID, "cdt_traffic", cycle, map[string]any{"traffic": traffic})
	}
	return result
}

func (w *Worker) telegramInstanceTraffic(accounts []app.Account, now time.Time) (map[string]float64, map[string]bool) {
	used := map[string]float64{}
	available := map[string]bool{}
	month := now.Format("2006-01")
	for _, account := range accounts {
		key := account.GroupKey
		if key == "" {
			key = account.AccessKeyID + "|" + account.RegionID
		}
		traffic := account.TrafficUsed
		isAvailable := account.TrafficAPIStatus != "fallback_cdt"
		if account.TrafficAPIStatus == "fallback_cdt" && account.InstanceID != "" {
			if sample, err := w.Store.InstanceTrafficUsage(account.ID, account.InstanceID, month); err == nil && sample.LastSampleMS > 0 {
				traffic = sample.TrafficBytes / (1024 * 1024 * 1024)
				isAvailable = true
			} else {
				traffic = 0
			}
		}
		used[key] += traffic
		available[key] = available[key] || isAvailable
	}
	return used, available
}

func (w *Worker) telegramInstances(page int) string {
	accounts, _ := w.Store.LoadAccounts(false)
	if len(accounts) == 0 {
		return "🖥️ 实例管理\n\n暂无实例数据。"
	}
	pageSize := 6
	page, total := pageBounds(page, len(accounts), pageSize)
	running, starting, stopped, other := instanceStatusCounts(accounts)
	lines := []string{
		"🖥️ 实例管理",
		fmt.Sprintf("共 %d 台  ·  🟢 %d 运行  ·  ⏸️ %d 停止", len(accounts), running, stopped),
	}
	if starting+other > 0 {
		lines = append(lines, fmt.Sprintf("🔄 %d 台处理中/待同步", starting+other))
	}
	lines = append(lines, "", fmt.Sprintf("选择实例  ·  第 %d/%d 页", page, total))
	for _, account := range accounts[(page-1)*pageSize : minInt(page*pageSize, len(accounts))] {
		lines = append(lines, "", statusIcon(account.InstanceStatus)+" "+instanceDisplayName(account), "📍 "+firstNonEmpty(account.RegionID, "未知区域"), "📦 "+formatTraffic(account.TrafficUsed, account.MaxTraffic), "🌐 "+firstNonEmpty(account.PublicIP, "暂无公网 IP"))
	}
	return strings.Join(lines, "\n")
}

func (w *Worker) telegramInstance(id int64) string {
	a, err := w.Store.Account(id, false)
	if err != nil {
		return "🖥️ 实例不存在或已被清理。"
	}
	return fmt.Sprintf("🖥️ 实例详情\n\n%s\n%s %s  ·  %s\n\n🌐 公网 IP：%s\n⚙️ 规格：%s\n📦 实例流量：%s\n🆔 实例 ID：%s\n🕒 最后同步：%s", instanceDisplayName(*a), statusIcon(a.InstanceStatus), statusLabel(a.InstanceStatus), firstNonEmpty(a.RegionID, "未知区域"), firstNonEmpty(a.PublicIP, "暂无"), firstNonEmpty(a.InstanceType, "未知"), formatTraffic(a.TrafficUsed, a.MaxTraffic), firstNonEmpty(a.InstanceID, "未知"), telegramUpdatedAt(a.UpdatedAt))
}

func (w *Worker) telegramReleaseConfirm(id int64, ttl time.Duration) string {
	return fmt.Sprintf("⚠️ 确认释放实例？\n\n%s\n\n释放后 ECS 将被删除，托管 EIP 和 DDNS 会同步清理，操作不可恢复。\n请在 %d 秒内确认。", w.telegramInstance(id), int(ttl/time.Second))
}

func (w *Worker) refreshTelegramAccount(ctx context.Context, id int64) error {
	a, err := w.Store.Account(id, false)
	if err != nil {
		return fmt.Errorf("实例不存在")
	}
	client := w.clientForAccount(*a)
	if client == nil {
		return fmt.Errorf("云客户端未配置")
	}
	instance, err := client.DescribeInstance(ctx, a.RegionID, a.InstanceID)
	if err != nil {
		return err
	}
	a.InstanceStatus, a.PublicIP, a.PrivateIP, a.InstanceType, a.UpdatedAt, a.HealthStatus = instance.Status, instance.PublicIP, instance.PrivateIP, instance.InstanceType, time.Now().Unix(), "ok"
	if a.PublicIPMode == "eip" && a.EIPAddress != "" {
		a.PublicIP = a.EIPAddress
	}
	now := time.Now()
	traffic, trafficStatus, trafficMessage, trafficErr := w.refreshTraffic(ctx, client, *a, now)
	if trafficErr == nil {
		a.TrafficUsed = traffic
		a.TrafficAPIStatus = trafficStatus
		a.TrafficAPIMessage = trafficMessage
		a.ProtectionSuspended = false
		a.ProtectionSuspendReason = ""
		a.ProtectionNotifiedAt = 0
		_ = w.Store.AddTrafficHistory(a.ID, traffic, now)
	} else {
		a.TrafficAPIStatus = "error"
		a.TrafficAPIMessage = cmsTrafficErrorMessage(trafficErr)
		a.ProtectionSuspended = true
		a.ProtectionSuspendReason = "traffic_api_error"
	}
	if err := w.Store.UpsertAccount(*a); err != nil {
		return err
	}
	if trafficErr != nil {
		return fmt.Errorf("%s", cmsTrafficErrorMessage(trafficErr))
	}
	return nil
}

func (w *Worker) refreshAllTelegramData(ctx context.Context) error {
	accounts, err := w.Store.LoadAccounts(false)
	if err != nil {
		return err
	}
	var failures []string
	for _, account := range accounts {
		if account.InstanceID == "" {
			continue
		}
		if err := w.refreshTelegramAccount(ctx, account.ID); err != nil {
			failures = append(failures, account.InstanceID+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func groupTrafficUsed(accounts []app.Account) map[string]float64 {
	type metric struct {
		used         float64
		fallbackUsed float64
		hasFallback  bool
	}
	metrics := map[string]*metric{}
	for _, account := range accounts {
		key := account.GroupKey
		if key == "" {
			key = account.AccessKeyID + "|" + account.RegionID
		}
		m := metrics[key]
		if m == nil {
			m = &metric{}
			metrics[key] = m
		}
		if account.TrafficAPIStatus == "fallback_cdt" {
			m.hasFallback = true
			if account.TrafficUsed > m.fallbackUsed {
				m.fallbackUsed = account.TrafficUsed
			}
		} else {
			m.used += account.TrafficUsed
		}
	}
	used := make(map[string]float64, len(metrics))
	for key, metric := range metrics {
		used[key] = metric.used
		if metric.hasFallback {
			used[key] = metric.fallbackUsed
		}
	}
	return used
}

func (w *Worker) controlTelegramAccount(ctx context.Context, id int64, action string) error {
	a, err := w.Store.Account(id, false)
	if err != nil {
		return fmt.Errorf("实例不存在")
	}
	client := w.clientForAccount(*a)
	if client == nil {
		return fmt.Errorf("云客户端未配置")
	}
	if action == "start" {
		if a.InstanceStatus != "Stopped" {
			return fmt.Errorf("当前状态为 %s", statusLabel(a.InstanceStatus))
		}
		err = client.StartInstance(ctx, a.RegionID, a.InstanceID)
		if err == nil {
			a.InstanceStatus = "Starting"
		}
	} else {
		if a.InstanceStatus != "Running" {
			return fmt.Errorf("当前状态为 %s", statusLabel(a.InstanceStatus))
		}
		err = client.StopInstance(ctx, a.RegionID, a.InstanceID, app.ResolveShutdownMode(a.StoppedMode, w.Store.GetSetting("shutdown_mode", "KeepCharging")))
		if err == nil {
			a.InstanceStatus = "Stopping"
		}
	}
	if err != nil {
		return err
	}
	a.AutoStartBlocked = action == "stop"
	a.ScheduleStopActive = false
	return w.Store.UpsertAccount(*a)
}

func (w *Worker) enqueueTelegramDelete(id int64) error {
	a, err := w.Store.Account(id, false)
	if err != nil {
		return fmt.Errorf("实例不存在")
	}
	if a.InstanceStatus == "Releasing" {
		return nil
	}
	if err := w.Store.MarkReleasing(id); err != nil {
		return err
	}
	if err := w.Store.EnqueueJob(randomActionToken(), "delete_instance", strconv.FormatInt(id, 10), map[string]any{"accountId": id}); err != nil {
		_ = w.Store.SetInstanceStatus(id, a.InstanceStatus)
		return err
	}
	return nil
}

func (w *Worker) clientForAccount(a app.Account) cloud.Client {
	return w.cloudClient(app.AccountGroup{AccessKeyID: a.AccessKeyID, AccessKeySecret: a.AccessKeySecret, RegionID: a.RegionID, SiteType: a.SiteType})
}

func (w *Worker) telegramConfirmTTL() time.Duration {
	seconds := int(numberValue(w.Store.GetSetting("notify_tg_confirm_ttl", "60")))
	if seconds < 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func randomActionToken() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func messageMap(m map[string]any, key, nested string) any {
	child, _ := m[key].(map[string]any)
	return child[nested]
}

func numberValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func stringValue(v any) string {
	if v == nil {
		return ""
	}
	if number, ok := v.(float64); ok {
		// Telegram IDs arrive as JSON numbers. Format integer IDs in decimal;
		// fmt.Sprint can turn large values into scientific notation.
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func intValueAt(parts []string, index int) int {
	if index >= len(parts) {
		return 1
	}
	n, _ := strconv.Atoi(parts[index])
	return n
}

func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
func splitIDs(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' || r == '，' || r == '；' || r == ' ' || r == '\t' })
}
func statusLabel(status string) string {
	if label, ok := map[string]string{"Running": "运行中", "Starting": "启动中", "Stopping": "停机中", "Stopped": "已停止", "Releasing": "释放中", "Released": "已释放"}[status]; ok {
		return label
	}
	return "未知"
}
func statusIcon(status string) string {
	switch status {
	case "Running":
		return "🟢"
	case "Starting", "Stopping", "Releasing":
		return "🔄"
	case "Stopped":
		return "⏸️"
	case "Released":
		return "⚫"
	default:
		return "⚪"
	}
}
func statusIconForTraffic(status string) string {
	switch status {
	case "正常":
		return "✅"
	case "接近阈值":
		return "⚠️"
	default:
		return "🛑"
	}
}
func instanceStatusCounts(accounts []app.Account) (running, processing, stopped, other int) {
	for _, account := range accounts {
		switch account.InstanceStatus {
		case "Running":
			running++
		case "Starting", "Stopping", "Releasing":
			processing++
		case "Stopped":
			stopped++
		default:
			other++
		}
	}
	return
}
func instanceDisplayName(account app.Account) string {
	return firstNonEmpty(account.Remark, account.InstanceName, account.InstanceID, "未命名实例")
}
func formatTraffic(used, max float64) string {
	if max > 0 {
		return fmt.Sprintf("实例流量 %.2f / %.2f GB", used, max)
	}
	return fmt.Sprintf("实例流量 %.2f GB", used)
}
func telegramUpdatedAt(timestamp int64) string {
	if timestamp <= 0 {
		return "未同步"
	}
	return time.Unix(timestamp, 0).Format("01-02 15:04")
}
func actionLabel(action string) string {
	if action == "start" {
		return "开机"
	}
	return "停机"
}
func pageBounds(page, count, size int) (int, int) {
	total := (count + size - 1) / size
	if total < 1 {
		total = 1
	}
	if page < 1 {
		page = 1
	}
	if page > total {
		page = total
	}
	return page, total
}

func (w *Worker) mainKeyboard() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "📊 流量总览", "callback_data": "m:traffic"}, {"text": "🖥️ 实例管理", "callback_data": "m:list:1"}}, {{"text": "🔄 刷新全部", "callback_data": "m:refreshall"}, {"text": "❔ 使用说明", "callback_data": "m:help"}}}}
}
func trafficKeyboard() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "🔄 刷新流量", "callback_data": "m:traffic"}, {"text": "🖥️ 实例管理", "callback_data": "m:list:1"}}, {{"text": "🏠 主菜单", "callback_data": "m:home"}}}}
}
func releaseKeyboard(token string, id int64, page ...int) map[string]any {
	currentPage := 1
	if len(page) > 0 {
		currentPage = maxInt(1, page[0])
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "⚠️ 确认释放", "callback_data": "m:confirm:" + token}, {"text": "取消", "callback_data": "m:cancel:" + token}}, {{"text": "↩️ 返回详情", "callback_data": fmt.Sprintf("m:inst:%d:%d", id, currentPage)}}}}
}

func (w *Worker) instancesKeyboard(page int) map[string]any {
	accounts, _ := w.Store.LoadAccounts(false)
	page, total := pageBounds(page, len(accounts), 6)
	keyboard := [][]map[string]string{}
	for _, account := range accounts[(page-1)*6 : minInt(page*6, len(accounts))] {
		keyboard = append(keyboard, []map[string]string{{"text": statusIcon(account.InstanceStatus) + " " + instanceDisplayName(account), "callback_data": fmt.Sprintf("m:inst:%d:%d", account.ID, page)}})
	}
	pager := []map[string]string{}
	if page > 1 {
		pager = append(pager, map[string]string{"text": "⬅️ 上一页", "callback_data": fmt.Sprintf("m:list:%d", page-1)})
	}
	if page < total {
		pager = append(pager, map[string]string{"text": "下一页 ➡️", "callback_data": fmt.Sprintf("m:list:%d", page+1)})
	}
	if len(pager) > 0 {
		keyboard = append(keyboard, pager)
	}
	keyboard = append(keyboard, []map[string]string{{"text": "📊 流量总览", "callback_data": "m:traffic"}, {"text": "🏠 主菜单", "callback_data": "m:home"}})
	return map[string]any{"inline_keyboard": keyboard}
}

func (w *Worker) instanceKeyboard(id int64, page ...int) map[string]any {
	a, err := w.Store.Account(id, false)
	if err != nil {
		return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "↩️ 返回实例列表", "callback_data": "m:list:1"}}}}
	}
	currentPage := 1
	if len(page) > 0 {
		currentPage = maxInt(1, page[0])
	}
	keyboard := [][]map[string]string{}
	if a.InstanceStatus == "Stopped" {
		keyboard = append(keyboard, []map[string]string{{"text": "🚀 开机", "callback_data": fmt.Sprintf("m:start:%d:%d", id, currentPage)}, {"text": "🔄 刷新", "callback_data": fmt.Sprintf("m:refresh:%d:%d", id, currentPage)}})
	} else if a.InstanceStatus == "Running" {
		keyboard = append(keyboard, []map[string]string{{"text": "🛑 停机", "callback_data": fmt.Sprintf("m:stop:%d:%d", id, currentPage)}, {"text": "🔄 刷新", "callback_data": fmt.Sprintf("m:refresh:%d:%d", id, currentPage)}})
	} else {
		keyboard = append(keyboard, []map[string]string{{"text": "🔄 刷新状态", "callback_data": fmt.Sprintf("m:refresh:%d:%d", id, currentPage)}})
	}
	if a.InstanceStatus != "Releasing" {
		keyboard = append(keyboard, []map[string]string{{"text": "🗑️ 释放实例", "callback_data": fmt.Sprintf("m:release:%d:%d", id, currentPage)}})
	}
	keyboard = append(keyboard, []map[string]string{{"text": "↩️ 实例列表", "callback_data": fmt.Sprintf("m:list:%d", currentPage)}, {"text": "🏠 主菜单", "callback_data": "m:home"}})
	return map[string]any{"inline_keyboard": keyboard}
}
