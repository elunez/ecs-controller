package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
)

const monthlyBillingAnalysisCacheType = "bill_monthly_analysis_v1"

type billingAnalysisTarget struct {
	Group     app.AccountGroup
	AccountID int64
	Label     string
}

type billingAnalysisAccount struct {
	GroupKey      string                `json:"group_key"`
	Label         string                `json:"label"`
	SiteType      string                `json:"site_type"`
	Currency      string                `json:"currency"`
	MonthlyCost   float64               `json:"monthly_cost"`
	Balance       float64               `json:"balance"`
	DailyAverage  float64               `json:"daily_average"`
	Forecast      float64               `json:"forecast"`
	AvailableDays *float64              `json:"available_days"`
	Categories    []billingCategoryItem `json:"categories"`
	UpdatedAt     string                `json:"updated_at"`
	Error         string                `json:"error,omitempty"`
}

type billingCategoryItem struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
}

func (s *Server) billingAnalysis(w http.ResponseWriter, r *http.Request) {
	if s.Store.GetSetting("enable_billing", "0") != "1" {
		s.json(w, http.StatusOK, map[string]any{"success": true, "enabled": false, "accounts": []any{}})
		return
	}
	targets, err := s.billingAnalysisTargets()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "费用分析账号读取失败")
		return
	}
	now := time.Now()
	cycle := now.Format("2006-01")
	refresh := truthy(r.URL.Query().Get("refresh"))
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	results := make(chan billingAnalysisAccount, len(targets))
	semaphore := make(chan struct{}, 3)
	var wait sync.WaitGroup
	for _, target := range targets {
		target := target
		wait.Add(1)
		go func() {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			results <- s.analyzeBillingAccount(ctx, target, cycle, now, refresh)
		}()
	}
	wait.Wait()
	close(results)
	items := make([]billingAnalysisAccount, 0, len(targets))
	for item := range results {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })

	summary := make(map[string]map[string]float64)
	for _, item := range items {
		if item.Error != "" {
			continue
		}
		currency := strings.ToUpper(fallback(item.Currency, "CNY"))
		if summary[currency] == nil {
			summary[currency] = map[string]float64{}
		}
		summary[currency]["monthly_cost"] += item.MonthlyCost
		summary[currency]["balance"] += item.Balance
		summary[currency]["forecast"] += item.Forecast
	}
	s.json(w, http.StatusOK, map[string]any{
		"success":           true,
		"enabled":           true,
		"cycle":             cycle,
		"days_elapsed":      now.Day(),
		"days_in_month":     time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day(),
		"cost_threshold":    numberString(s.Store.GetSetting("billing_cost_threshold", ""), 0),
		"balance_threshold": numberString(s.Store.GetSetting("billing_balance_threshold", ""), 0),
		"summary":           summary,
		"accounts":          items,
	})
}

func (s *Server) billingAnalysisTargets() ([]billingAnalysisTarget, error) {
	groups, err := s.Store.LoadGroups()
	if err != nil {
		return nil, err
	}
	accounts, err := s.Store.LoadAccounts(false)
	if err != nil {
		return nil, err
	}
	accountByGroup := make(map[string]app.Account)
	for _, account := range accounts {
		if _, exists := accountByGroup[account.GroupKey]; !exists {
			accountByGroup[account.GroupKey] = account
		}
	}
	seen := make(map[string]struct{})
	targets := make([]billingAnalysisTarget, 0, len(groups))
	for _, group := range groups {
		account, exists := accountByGroup[group.GroupKey]
		if !exists || strings.TrimSpace(group.AccessKeyID) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(group.SiteType)) + "\x00" + strings.TrimSpace(group.AccessKeyID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		label := strings.TrimSpace(group.Remark)
		if label == "" {
			label = "云账号 " + fmt.Sprint(len(targets)+1)
		}
		targets = append(targets, billingAnalysisTarget{Group: group, AccountID: account.ID, Label: label})
	}
	return targets, nil
}

func (s *Server) analyzeBillingAccount(ctx context.Context, target billingAnalysisTarget, cycle string, now time.Time, refresh bool) billingAnalysisAccount {
	item := billingAnalysisAccount{GroupKey: target.Group.GroupKey, Label: target.Label, SiteType: target.Group.SiteType, Currency: map[bool]string{true: "USD", false: "CNY"}[target.Group.SiteType == "international"], Categories: []billingCategoryItem{}}
	client := s.cloudClient(app.Account{AccessKeyID: target.Group.AccessKeyID, AccessKeySecret: target.Group.AccessKeySecret, RegionID: target.Group.RegionID, SiteType: target.Group.SiteType})
	billingClient, ok := client.(cloud.BillingClient)
	if client == nil || !ok {
		item.Error = "当前云账号不支持费用查询"
		return item
	}
	cacheAge := 6 * time.Hour
	if refresh {
		cacheAge = 0
	}
	var failures []string
	if cached, cacheOK := s.Store.GetBillingCache(target.AccountID, "bill_overview", cycle, cacheAge); cacheOK {
		item.MonthlyCost = numberFloat(cached["monthly_cost"])
		item.Currency = fallback(stringValue(cached["currency"]), item.Currency)
	} else if total, currency, err := billingClient.GetBillOverview(ctx, target.Group.SiteType, cycle); err != nil {
		failures = append(failures, "本月消费不可用")
	} else {
		item.MonthlyCost, item.Currency = total, fallback(currency, item.Currency)
		_ = s.Store.SetBillingCache(target.AccountID, "bill_overview", cycle, map[string]any{"monthly_cost": total, "currency": item.Currency})
	}
	if cached, cacheOK := s.Store.GetBillingCache(target.AccountID, "balance", "", cacheAge); cacheOK {
		item.Balance = numberFloat(cached["balance"])
		item.Currency = fallback(stringValue(cached["currency"]), item.Currency)
	} else if balance, currency, err := billingClient.GetAccountBalance(ctx, target.Group.SiteType); err != nil {
		failures = append(failures, "账户余额不可用")
	} else {
		item.Balance, item.Currency = balance, fallback(currency, item.Currency)
		_ = s.Store.SetBillingCache(target.AccountID, "balance", "", map[string]any{"balance": balance, "currency": item.Currency})
	}

	var details []cloud.BillingDetail
	if cached, cacheOK := s.Store.GetBillingCache(target.AccountID, monthlyBillingAnalysisCacheType, cycle, cacheAge); cacheOK {
		details, _ = decodeBillingDetails(cached["items"])
	} else if detailClient, ok := client.(cloud.MonthlyBillingDetailClient); ok {
		var detailErr error
		details, detailErr = detailClient.GetMonthlyBillingDetails(ctx, target.Group.SiteType, cycle)
		if detailErr != nil {
			failures = append(failures, "费用分项不可用")
		} else {
			_ = s.Store.SetBillingCache(target.AccountID, monthlyBillingAnalysisCacheType, cycle, map[string]any{"items": details, "currency": item.Currency})
		}
	}
	item.Categories = summarizeBillingCategories(details)
	daysElapsed := maxInt(now.Day(), 1)
	daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, now.Location()).Day()
	item.DailyAverage = item.MonthlyCost / float64(daysElapsed)
	item.Forecast = item.DailyAverage * float64(daysInMonth)
	if item.DailyAverage > 0 {
		available := item.Balance / item.DailyAverage
		item.AvailableDays = &available
	}
	item.UpdatedAt = now.Format("2006-01-02 15:04:05")
	item.Error = strings.Join(failures, "；")
	return item
}

func summarizeBillingCategories(details []cloud.BillingDetail) []billingCategoryItem {
	totals := map[string]float64{"instance": 0, "disk": 0, "eip": 0, "network": 0, "other": 0}
	for _, detail := range details {
		text := strings.ToLower(strings.Join([]string{detail.ProductName, detail.ProductCode, detail.ProductDetail, detail.BillingItem, detail.BillingItemCode}, " "))
		key := "other"
		switch {
		case containsAny(text, "cloud disk", "disk", "essd", "storage", "云盘", "磁盘"):
			key = "disk"
		case containsAny(text, "eip", "elastic ip", "弹性公网", "公网 ip"):
			key = "eip"
		case containsAny(text, "bandwidth", "traffic", "network", "cdt", "带宽", "流量", "网络"):
			key = "network"
		case containsAny(text, "ecs", "instance", "compute", "云服务器", "实例", "计算"):
			key = "instance"
		}
		totals[key] += detail.Amount
	}
	labels := []struct{ key, label string }{{"instance", "实例计算"}, {"disk", "云盘"}, {"eip", "EIP"}, {"network", "公网与流量"}, {"other", "其他"}}
	result := make([]billingCategoryItem, 0, len(labels))
	for _, label := range labels {
		if totals[label.key] != 0 {
			result = append(result, billingCategoryItem{Key: label.key, Label: label.label, Amount: totals[label.key]})
		}
	}
	return result
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
