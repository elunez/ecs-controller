package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
)

func (s *Server) syncGroup(groupKey string) (int, error) {
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	return s.syncGroupContext(rctx(), groupKey, false)
}

// SyncInventory reconciles all configured account groups in the background.
// Automatic reconciliation requires two successful missing observations before
// it starts the existing local cleanup flow for an externally removed ECS.
func (s *Server) SyncInventory(ctx context.Context) (int, error) {
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	groups, err := s.Store.LoadGroups()
	if err != nil {
		return 0, err
	}
	type groupResult struct {
		key   string
		count int
		err   error
	}
	semaphore := make(chan struct{}, 3)
	results := make(chan groupResult, len(groups))
	var wg sync.WaitGroup
	for _, group := range groups {
		groupKey := group.GroupKey
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			groupCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			count, syncErr := s.syncGroupContext(groupCtx, groupKey, true)
			results <- groupResult{key: groupKey, count: count, err: syncErr}
		}()
	}
	wg.Wait()
	close(results)

	total := 0
	var failures []string
	for result := range results {
		total += result.count
		if result.err != nil {
			failures = append(failures, result.key+": "+result.err.Error())
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return total, fmt.Errorf("部分账号组同步失败: %s", strings.Join(failures, "; "))
	}
	return total, nil
}

func (s *Server) syncGroupContext(ctx context.Context, groupKey string, confirmMissing bool) (int, error) {
	groups, err := s.Store.LoadGroups()
	if err != nil {
		return 0, err
	}
	var group *app.AccountGroup
	for i := range groups {
		if groups[i].GroupKey == groupKey {
			group = &groups[i]
			break
		}
	}
	if group == nil {
		return 0, fmt.Errorf("账号组不存在")
	}
	client := s.cloudClient(app.Account{AccessKeyID: group.AccessKeyID, AccessKeySecret: group.AccessKeySecret, RegionID: group.RegionID, SiteType: group.SiteType})
	if client == nil {
		return 0, fmt.Errorf("云客户端未配置")
	}
	instances, err := client.DescribeInstances(ctx, group.RegionID)
	if err != nil {
		return 0, err
	}
	accounts, err := s.Store.LoadAccounts(true)
	if err != nil {
		return 0, err
	}
	publicNetworks := map[string]cloud.InstancePublicNetwork{}
	publicNetworkSynced := false
	if networkClient, ok := client.(cloud.InstancePublicNetworkClient); ok {
		instanceIDs := make([]string, 0, len(instances))
		for _, instance := range instances {
			instanceIDs = append(instanceIDs, instance.ID)
		}
		if networks, networkErr := networkClient.DescribeInstancePublicNetworks(ctx, group.RegionID, instanceIDs); networkErr != nil {
			s.Log.Printf("同步实例公网带宽失败（账号组 %s）: %v", group.GroupKey, networkErr)
		} else {
			publicNetworks = networks
			publicNetworkSynced = true
		}
	}
	remoteIDs := make(map[string]bool, len(instances))
	count := 0
	for _, instance := range instances {
		remoteIDs[instance.ID] = true
		count++
		var existing *app.Account
		for i := range accounts {
			sameGroup := accounts[i].GroupKey == group.GroupKey || (accounts[i].AccessKeyID == group.AccessKeyID && accounts[i].RegionID == group.RegionID)
			if sameGroup && accounts[i].InstanceID == instance.ID {
				existing = &accounts[i]
				break
			}
		}
		if existing != nil {
			_ = s.Store.SetSetting(inventoryMissingKey(existing.ID), "0")
		}
		if existing != nil && (existing.IsDeleted != 0 || existing.InstanceStatus == "Releasing") {
			// A user-triggered release must not be resurrected by a manual sync
			// while the remote ECS record is still visible.
			continue
		}
		a := app.Account{AccessKeyID: group.AccessKeyID, AccessKeySecret: group.AccessKeySecret, RegionID: group.RegionID, InstanceID: instance.ID, MaxTraffic: group.MaxTraffic, Remark: group.Remark, SiteType: group.SiteType, GroupKey: group.GroupKey, InstanceName: instance.Name, InstanceType: instance.InstanceType, InternetBandwidth: instance.InternetBandwidth, PublicIP: instance.PublicIP, PublicIPMode: "ecs_public_ip", PrivateIP: instance.PrivateIP, CPU: instance.CPU, Memory: instance.Memory, OSName: instance.OSName, InstanceStatus: instance.Status, HealthStatus: "ok", UpdatedAt: time.Now().Unix()}
		if network, hasEIP := publicNetworks[instance.ID]; hasEIP {
			a.PublicIPMode = "eip"
			a.EIPAllocationID, a.EIPAddress = network.AllocationID, network.Address
			if network.Address != "" {
				a.PublicIP = network.Address
			}
			if network.Bandwidth > 0 {
				a.InternetBandwidth = network.Bandwidth
			}
		}
		if existing != nil {
			// Keep local runtime state (traffic, schedules, protection flags and
			// managed-network metadata) while refreshing cloud-owned fields.
			a.ID = existing.ID
			a.TrafficUsed, a.TrafficBillingMonth = existing.TrafficUsed, existing.TrafficBillingMonth
			a.LastKeepAliveAt, a.AutoStartBlocked = existing.LastKeepAliveAt, existing.AutoStartBlocked
			a.ScheduleLastStartDate, a.ScheduleLastStopDate = existing.ScheduleLastStartDate, existing.ScheduleLastStopDate
			a.ScheduleStopActive, a.ScheduleBlockedByTraffic = existing.ScheduleStopActive, existing.ScheduleBlockedByTraffic
			a.ScheduleEnabled, a.ScheduleStartEnabled, a.ScheduleStopEnabled = existing.ScheduleEnabled, existing.ScheduleStartEnabled, existing.ScheduleStopEnabled
			a.StartTime, a.StopTime = existing.StartTime, existing.StopTime
			a.StoppedMode = existing.StoppedMode
			a.TrafficAPIStatus, a.TrafficAPIMessage = existing.TrafficAPIStatus, existing.TrafficAPIMessage
			a.ProtectionSuspended, a.ProtectionSuspendReason, a.ProtectionNotifiedAt = existing.ProtectionSuspended, existing.ProtectionSuspendReason, existing.ProtectionNotifiedAt
			if network, hasEIP := publicNetworks[instance.ID]; hasEIP {
				// Only controller-created EIPs may be replaced from the UI.
				a.EIPManaged = existing.EIPManaged && existing.EIPAllocationID == network.AllocationID
			} else if !publicNetworkSynced {
				// A failed EIP lookup must not erase known network metadata.
				a.EIPAllocationID, a.EIPAddress, a.EIPManaged = existing.EIPAllocationID, existing.EIPAddress, existing.EIPManaged
				a.PublicIPMode = existing.PublicIPMode
				if a.InternetBandwidth < 1 {
					a.InternetBandwidth = existing.InternetBandwidth
				}
			}
			if a.PublicIPMode == "eip" && a.EIPAddress != "" {
				a.PublicIP = a.EIPAddress
			}
		}
		if err := s.Store.UpsertAccount(a); err != nil {
			return count, err
		}
	}
	for _, account := range accounts {
		if account.InstanceID == "" || account.IsDeleted != 0 || remoteIDs[account.InstanceID] {
			continue
		}
		sameGroup := account.GroupKey == group.GroupKey || (account.AccessKeyID == group.AccessKeyID && account.RegionID == group.RegionID)
		if !sameGroup {
			continue
		}
		missingKey := inventoryMissingKey(account.ID)
		if confirmMissing {
			missingCount, _ := strconv.Atoi(s.Store.GetSetting(missingKey, "0"))
			missingCount++
			if err := s.Store.SetSetting(missingKey, strconv.Itoa(missingCount)); err != nil {
				return count, err
			}
			if missingCount < 2 {
				s.Store.AddLog("warning", "账号清单同步首次未发现实例，等待下次确认: "+account.InstanceID)
				continue
			}
		}
		_ = s.Store.SetSetting(missingKey, "0")
		if account.InstanceStatus == "ReleaseFailed" {
			// A failed release is safe to forget only after a successful,
			// complete DescribeInstances response confirms this exact instance ID
			// no longer exists. Never use a reused IP address for this decision.
			if removed, removeErr := s.Store.PhysicallyDeleteReleaseFailed(account.ID); removeErr != nil {
				return count, removeErr
			} else if removed {
				s.Store.AddLog("info", "已清理云端不存在的释放失败残留记录: "+account.InstanceID)
			}
			continue
		}
		// The instance disappeared outside this controller. Hide the stale row
		// immediately and atomically queue EIP/DDNS/group cleanup. A terminal
		// cleanup failure restores it as ReleaseFailed for manual intervention.
		if err := s.Store.QueueMissingInstanceCleanup(account.ID, randomToken(16)); err != nil {
			return count, err
		}
		s.Store.AddLog("info", "已同步移除云端不存在的实例: "+account.InstanceID)
	}
	return count, nil
}

func inventoryMissingKey(accountID int64) string {
	return "inventory_missing_" + strconv.FormatInt(accountID, 10)
}
