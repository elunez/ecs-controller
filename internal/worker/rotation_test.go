package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/store"
)

func rotationTestAccounts(t *testing.T, s *store.Store) (app.Account, app.Account) {
	t.Helper()
	for _, account := range []app.Account{
		{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: "i-a", InstanceStatus: "Running", MaxTraffic: 200, ScheduleEnabled: true, ScheduleStartEnabled: true, ScheduleStopEnabled: true, StartTime: "00:00", StopTime: "12:00"},
		{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: "i-b", InstanceStatus: "Stopped", MaxTraffic: 200, ScheduleEnabled: true, ScheduleStartEnabled: true, ScheduleStopEnabled: true, StartTime: "12:00", StopTime: "23:00"},
	} {
		if err := s.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("load accounts: %#v %v", accounts, err)
	}
	byInstance := map[string]app.Account{}
	for _, account := range accounts {
		byInstance[account.InstanceID] = account
	}
	return byInstance["i-a"], byInstance["i-b"]
}

func saveRotationTestGroup(t *testing.T, s *store.Store, a, b app.Account) {
	t.Helper()
	if err := s.SaveRotationGroups([]app.RotationGroup{{
		ID: "group-1", Name: "轮转组", Domain: "service.example.com", Provider: "cloudflare",
		AdvanceStartMinutes: 10, DelayStopMinutes: 5,
		DNS: app.DNSCredentials{CloudflareToken: "token", TTL: 600},
		Members: []app.RotationMember{
			{AccountID: a.ID},
			{AccountID: b.ID},
		},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestRotationStopsOldInstanceEvenWhenNextStartFails(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b := rotationTestAccounts(t, s)
	saveRotationTestGroup(t, s, a, b)
	w := &Worker{Store: s}

	startCloud := &fakeCloud{startErr: errors.New("start failed")}
	now := time.Date(2026, 8, 23, 11, 50, 0, 0, time.Local)
	if !w.runRotationSchedule(context.Background(), startCloud, &b, now, "KeepCharging") || startCloud.started != 1 {
		t.Fatalf("next instance start was not attempted: %#v", startCloud)
	}

	stopCloud := &fakeCloud{}
	now = time.Date(2026, 8, 23, 12, 5, 0, 0, time.Local)
	if !w.runRotationSchedule(context.Background(), stopCloud, &a, now, "KeepCharging") || stopCloud.stopped != 1 || a.InstanceStatus != "Stopping" {
		t.Fatalf("old instance did not stop independently: cloud=%#v account=%#v", stopCloud, a)
	}
	states, err := s.RotationStates("group-1")
	if err != nil || states[a.ID].LastStopDate != "2026-08-23" {
		t.Fatalf("stop state was not persisted: %#v %v", states, err)
	}
}

func TestRotationDoesNotStartWhenTrafficProtectionIsActive(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b := rotationTestAccounts(t, s)
	saveRotationTestGroup(t, s, a, b)
	b.MaxTraffic = 100
	b.TrafficAPIStatus = "ok"
	cloudClient := &fakeCloud{cdtTraffic: 100}
	w := &Worker{Store: s}
	w.runCachedAutomation(context.Background(), cloudClient, &b, time.Date(2026, 8, 23, 11, 50, 0, 0, time.Local), 95, "stop_and_notify", "KeepCharging", false, false)
	if cloudClient.started != 0 {
		t.Fatalf("rotation bypassed traffic protection: %#v", cloudClient)
	}
}

func TestRotationDoesNotReplayMissedStartAfterWindowEnded(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b := rotationTestAccounts(t, s)
	saveRotationTestGroup(t, s, a, b)
	cloudClient := &fakeCloud{}
	w := &Worker{Store: s}
	w.runRotationSchedule(context.Background(), cloudClient, &b, time.Date(2026, 8, 23, 23, 10, 0, 0, time.Local), "KeepCharging")
	if cloudClient.started != 0 {
		t.Fatalf("missed start was replayed after stop time: %#v", cloudClient)
	}
	states, err := s.RotationStates("group-1")
	if err != nil || states[b.ID].LastStartDate != "2026-08-23" || states[b.ID].LastStopDate != "2026-08-23" {
		t.Fatalf("missed window was not retired: %#v %v", states, err)
	}
}

func TestRotationDueAcrossMidnight(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	tests := []struct {
		name       string
		now        time.Time
		clock      string
		offset     int
		expectDate string
	}{
		{"advance to previous day", time.Date(2026, 8, 22, 23, 55, 0, 0, location), "00:05", -10, "2026-08-23"},
		{"delay to next day", time.Date(2026, 8, 24, 0, 5, 0, 0, location), "23:55", 10, "2026-08-23"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			date, due := rotationDue(test.now, test.clock, test.offset, "")
			if !due || date != test.expectDate {
				t.Fatalf("date=%q due=%v", date, due)
			}
			if _, repeated := rotationDue(test.now.Add(time.Minute), test.clock, test.offset, date); repeated {
				t.Fatal("same occurrence ran twice")
			}
		})
	}
}
