package store

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func TestSecretsAndAccountsAreEncrypted(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sealed, err := s.Seal("secret-value")
	if err != nil {
		t.Fatal(err)
	}
	if sealed == "secret-value" {
		t.Fatal("secret was not encrypted")
	}
	plain, err := s.OpenSecret(sealed)
	if err != nil || plain != "secret-value" {
		t.Fatalf("decrypt: %q, %v", plain, err)
	}

	groups := []app.AccountGroup{{GroupKey: "group-a", AccessKeyID: "LTAI-test", AccessKeySecret: "secret-value", RegionID: "cn-hongkong", MaxTraffic: 200}}
	if err := s.SaveGroups(groups); err != nil {
		t.Fatal(err)
	}
	if raw := s.GetSetting("account_groups", ""); raw == "" || contains(raw, "secret-value") {
		t.Fatalf("plaintext group secret leaked: %s", raw)
	}
	loaded, err := s.LoadGroups()
	if err != nil || len(loaded) != 1 || loaded[0].AccessKeySecret != "secret-value" {
		t.Fatalf("load group: %#v, %v", loaded, err)
	}
	if err := s.SaveGroups([]app.AccountGroup{{GroupKey: "group-a", AccessKeyID: "LTAI-test", AccessKeySecret: "********", RegionID: "cn-hongkong", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	loaded, err = s.LoadGroups()
	if err != nil || len(loaded) != 1 || loaded[0].AccessKeySecret != "secret-value" {
		t.Fatalf("masked group secret was lost: %#v, %v", loaded, err)
	}

	if err := s.UpsertAccount(app.Account{AccessKeyID: "LTAI-test", AccessKeySecret: "secret-value", RegionID: "cn-hongkong", GroupKey: "group-a", InstanceID: "i-test", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].AccessKeySecret != "secret-value" {
		t.Fatalf("load account: %#v, %v", accounts, err)
	}
}

func TestSaveGroupsRejectsDuplicateAccountRegion(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	duplicate := []app.AccountGroup{
		{GroupKey: "group-hk-a", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong"},
		{GroupKey: "group-hk-b", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "CN-HONGKONG"},
	}
	if err := s.SaveGroups(duplicate); err == nil {
		t.Fatal("duplicate account and region was accepted")
	}

	differentRegions := []app.AccountGroup{
		{GroupKey: "group-hk", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong"},
		{GroupKey: "group-sg", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "ap-southeast-1"},
	}
	if err := s.SaveGroups(differentRegions); err != nil {
		t.Fatalf("same account in different regions was rejected: %v", err)
	}
	loaded, err := s.LoadGroups()
	if err != nil || len(loaded) != 2 {
		t.Fatalf("different region groups were not saved: %#v, %v", loaded, err)
	}
}

func TestPasskeyCredentialsAndChallengesAreEncryptedAndOneTime(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	credentialData := `{"id":"credential","publicKey":"key"}`
	if err := s.SavePasskeyCredential("credential-id", credentialData); err != nil {
		t.Fatal(err)
	}
	var rawCredential string
	if err := s.DB.QueryRow(`SELECT credential_data FROM passkey_credentials WHERE credential_id=?`, "credential-id").Scan(&rawCredential); err != nil {
		t.Fatal(err)
	}
	if rawCredential == credentialData || !strings.HasPrefix(rawCredential, "ENC1") {
		t.Fatalf("passkey credential was not encrypted: %q", rawCredential)
	}
	credentials, err := s.PasskeyCredentials()
	if err != nil || len(credentials) != 1 || credentials[0].Data != credentialData {
		t.Fatalf("passkey credential round trip: %#v, %v", credentials, err)
	}

	if err := s.SavePasskeyChallenge("challenge-id", "login", "", `{"challenge":"value"}`, time.Minute); err != nil {
		t.Fatal(err)
	}
	var rawChallenge string
	if err := s.DB.QueryRow(`SELECT session_data FROM passkey_challenges WHERE id=?`, "challenge-id").Scan(&rawChallenge); err != nil {
		t.Fatal(err)
	}
	if rawChallenge == `{"challenge":"value"}` || !strings.HasPrefix(rawChallenge, "ENC1") {
		t.Fatalf("passkey challenge was not encrypted: %q", rawChallenge)
	}
	data, ok, err := s.ConsumePasskeyChallenge("challenge-id", "login", "")
	if err != nil || !ok || data != `{"challenge":"value"}` {
		t.Fatalf("passkey challenge round trip: data=%q ok=%v err=%v", data, ok, err)
	}
	if _, ok, err := s.ConsumePasskeyChallenge("challenge-id", "login", ""); err != nil || ok {
		t.Fatalf("passkey challenge was reusable: ok=%v err=%v", ok, err)
	}
}

func TestExpiredPasskeyChallengeCannotBeConsumed(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SavePasskeyChallenge("expired", "register", "session", `{"challenge":"expired"}`, -time.Second); err != nil {
		t.Fatal(err)
	}
	if data, ok, err := s.ConsumePasskeyChallenge("expired", "register", "session"); err != nil || ok || data != "" {
		t.Fatalf("expired passkey challenge was accepted: data=%q ok=%v err=%v", data, ok, err)
	}
}

func contains(value, needle string) bool {
	return len(needle) > 0 && len(value) >= len(needle) && stringIndex(value, needle) >= 0
}

func TestAdminCredentialsRequireUsernameAndPassword(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SetAdminPassword("secret1"); err != nil {
		t.Fatal(err)
	}
	if s.IsInitialized() {
		t.Fatal("password without username initialized the store")
	}

	for _, username := range []string{"ab", "admin name", "管理员", "admin@ecs", strings.Repeat("a", 33)} {
		if err := s.SetAdminCredentials(username, "secret1"); err == nil {
			t.Fatalf("invalid username %q was accepted", username)
		}
	}
	if err := s.SetAdminCredentials("admin.test-1", "secret1"); err != nil {
		t.Fatal(err)
	}
	if !s.IsInitialized() {
		t.Fatal("valid credentials did not initialize the store")
	}
	if !s.CheckAdminCredentials("admin.test-1", "secret1") {
		t.Fatal("valid username and password were rejected")
	}
	if s.CheckAdminCredentials("Admin.test-1", "secret1") {
		t.Fatal("username comparison was not case-sensitive")
	}
	if s.CheckAdminCredentials("admin.test-1", "wrong1") {
		t.Fatal("wrong password was accepted")
	}
}

func TestUpsertAccountUpdatesRuntimeState(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g", InstanceID: "i-1", MaxTraffic: 200, TrafficUsed: 12.5, TrafficBillingMonth: "2026-07", InstanceStatus: "Running", UpdatedAt: 100, LastKeepAliveAt: 90, AutoStartBlocked: true, ScheduleLastStartDate: "2026-07-01", ScheduleLastStopDate: "2026-07-02", ScheduleStopActive: true, ScheduleBlockedByTraffic: true, HealthStatus: "ok", TrafficAPIStatus: "ok", ProtectionSuspended: true, ProtectionSuspendReason: "credential_invalid", ProtectionNotifiedAt: 101}
	if err := s.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	items, err := s.LoadAccounts(false)
	if err != nil || len(items) != 1 {
		t.Fatalf("load account: %#v %v", items, err)
	}
	account.ID = items[0].ID
	account.TrafficUsed = 88.5
	account.InstanceStatus = "Stopping"
	account.UpdatedAt = 200
	account.LastKeepAliveAt = 190
	account.AutoStartBlocked = false
	account.ScheduleStopActive = false
	account.ScheduleBlockedByTraffic = false
	account.ProtectionSuspended = false
	account.ProtectionSuspendReason = ""
	if err := s.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Account(account.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TrafficUsed != 88.5 || updated.InstanceStatus != "Stopping" || updated.LastKeepAliveAt != 190 || updated.AutoStartBlocked || updated.ScheduleStopActive || updated.ScheduleBlockedByTraffic || updated.ProtectionSuspended {
		t.Fatalf("runtime state was not updated: %#v", updated)
	}
}

func TestLoadAccountsByIDDesc(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, instanceID := range []string{"i-first", "i-second", "i-third"} {
		if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g", InstanceID: instanceID}); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := s.LoadAccountsByIDDesc(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 || accounts[0].InstanceID != "i-third" || accounts[1].InstanceID != "i-second" || accounts[2].InstanceID != "i-first" {
		t.Fatalf("accounts are not ordered by id desc: %#v", accounts)
	}
}

func TestDisablingScheduledStopClearsItsAutomationBlock(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "g", InstanceID: "i-1", ScheduleEnabled: true, ScheduleStopEnabled: true, ScheduleStopActive: true, InstanceStatus: "Stopped"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	if err := s.UpdateInstanceSchedule(accounts[0].ID, false, "", ""); err != nil {
		t.Fatal(err)
	}
	accounts, err = s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].ScheduleStopActive {
		t.Fatalf("scheduled-stop block was not cleared: %#v %v", accounts, err)
	}
}

func TestRemoveAccountsOutsideGroupsOnlyRemovesLocalRecords(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "removed", InstanceID: "i-removed", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	removed, err := s.RemoveAccountsOutsideGroups([]app.AccountGroup{{GroupKey: "kept", AccessKeyID: "other", RegionID: "cn-test"}})
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed=%d err=%v", len(removed), err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("removed account remains visible: %#v", accounts)
	}
	all, err := s.LoadAccounts(true)
	if err != nil || len(all) != 1 || all[0].InstanceStatus != "Released" || all[0].IsDeleted != 2 {
		t.Fatalf("local record was not retained as released: %#v %v", all, err)
	}
}

func TestLegacyTrafficStatsAndMonthlyReset(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := []string{
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY AUTOINCREMENT, access_key_id TEXT, access_key_secret TEXT, region_id TEXT, instance_id TEXT, max_traffic REAL, schedule_enabled INTEGER DEFAULT 0, start_time TEXT, stop_time TEXT, traffic_used REAL DEFAULT 0, instance_status TEXT DEFAULT 'Unknown', updated_at INTEGER DEFAULT 0, last_keep_alive_at INTEGER DEFAULT 0, is_deleted INTEGER DEFAULT 0)`,
		`CREATE TABLE traffic_hourly (id INTEGER PRIMARY KEY AUTOINCREMENT, access_key_id TEXT, traffic REAL, recorded_at INTEGER)`,
		`CREATE TABLE traffic_daily (id INTEGER PRIMARY KEY AUTOINCREMENT, access_key_id TEXT, traffic REAL, recorded_at INTEGER)`,
	}
	for _, statement := range legacySchema {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO accounts(id,access_key_id,access_key_secret,region_id,instance_id,max_traffic,traffic_used,is_deleted) VALUES(1,'ak-legacy','legacy-secret','cn-test','i-legacy',200,12,0)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES('notify_password','legacy-mail-password')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO traffic_hourly(access_key_id,traffic,recorded_at) VALUES('ak-legacy',3.5,100)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO traffic_daily(access_key_id,traffic,recorded_at) VALUES('ak-legacy',7.25,200)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var hourly, daily float64
	if err := s.DB.QueryRow(`SELECT traffic FROM traffic_hourly WHERE account_id=1`).Scan(&hourly); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT traffic FROM traffic_daily WHERE account_id=1`).Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if hourly != 3.5 || daily != 7.25 {
		t.Fatalf("legacy statistics were not migrated: hourly=%v daily=%v", hourly, daily)
	}
	var rawSecret, rawNotify string
	if err := s.DB.QueryRow(`SELECT access_key_secret FROM accounts WHERE id=1`).Scan(&rawSecret); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT value FROM settings WHERE key='notify_password'`).Scan(&rawNotify); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawSecret, "ENC1") || !strings.HasPrefix(rawNotify, "ENC1") {
		t.Fatalf("legacy secrets were not encrypted: account=%q notify=%q", rawSecret, rawNotify)
	}
	groups, err := s.LoadGroups()
	if err != nil || len(groups) != 1 || groups[0].GroupKey != derivedGroupKey("ak-legacy", "cn-test") || groups[0].AccessKeySecret != "legacy-secret" {
		t.Fatalf("legacy account group was not derived: %#v %v", groups, err)
	}
	if _, err := s.DB.Exec(`UPDATE accounts SET traffic_billing_month=?,traffic_used=99,schedule_blocked_by_traffic=1`, "2000-01"); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("reloaded accounts: %#v %v", accounts, err)
	}
	if accounts[0].TrafficUsed != 0 || accounts[0].TrafficBillingMonth != time.Now().Format("2006-01") || accounts[0].ScheduleBlockedByTraffic {
		t.Fatalf("monthly traffic was not reset: %#v", accounts[0])
	}
}

func TestDailyTrafficDeltaUsesCumulativeSnapshots(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: "i-1", TrafficAPIStatus: "ok"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	day := time.Date(2026, 7, 31, 12, 0, 0, 0, time.Local)
	if err := s.AddTrafficHistory(accounts[0].ID, 10, day.Add(-13*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTrafficHistory(accounts[0].ID, 14, day); err != nil {
		t.Fatal(err)
	}
	traffic, complete, err := s.DailyTrafficDelta(accounts[0].ID, day)
	if err != nil || !complete || traffic != 4 {
		t.Fatalf("daily traffic delta: traffic=%v complete=%v err=%v", traffic, complete, err)
	}

	if err := s.AddTrafficHistory(accounts[0].ID, 3, day.AddDate(0, 0, 1).Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, complete, err := s.DailyTrafficDelta(accounts[0].ID, day.AddDate(0, 0, 1)); err != nil || complete {
		t.Fatalf("counter reset should be incomplete: complete=%v err=%v", complete, err)
	}
}

func TestLegacyAdminPasswordFormatsUpgradeOnLogin(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	password := "legacy password 123"
	if err := s.SetSetting("admin_password", password); err != nil {
		t.Fatal(err)
	}
	if !s.CheckAdminPassword(password) || s.GetSetting("admin_password", "") == password || s.CheckAdminPassword("wrong password 123") {
		t.Fatal("plaintext password was not upgraded safely")
	}

	bcryptHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	phpBcrypt := "$2y$" + string(bcryptHash)[4:]
	if err := s.SetSetting("admin_password", phpBcrypt); err != nil {
		t.Fatal(err)
	}
	if !s.CheckAdminPassword(password) {
		t.Fatal("PHP bcrypt password was not accepted")
	}

	salt := []byte("0123456789abcdef")
	argonHash := argon2.IDKey([]byte(password), salt, 1, 8192, 1, 32)
	argonEncoded := fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(argonHash))
	if err := s.SetSetting("admin_password", argonEncoded); err != nil {
		t.Fatal(err)
	}
	if !s.CheckAdminPassword(password) || s.GetSetting("admin_password", "") == argonEncoded {
		t.Fatal("PHP Argon2 password was not accepted and upgraded")
	}
}

func TestRotationGroupsEncryptSecretsAndPreserveMaskedValues(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	group := app.RotationGroup{
		ID: "group-1", Name: "轮转组", Domain: "service.example.com", Provider: "dnspod",
		DNS:     app.DNSCredentials{DNSPodSecretID: "secret-id", DNSPodSecretKey: "secret-key", TTL: 600},
		Members: []app.RotationMember{{AccountID: 1}, {AccountID: 2}},
	}
	if err := s.SaveRotationGroups([]app.RotationGroup{group}); err != nil {
		t.Fatal(err)
	}
	if raw := s.GetSetting("rotation_groups", ""); strings.Contains(raw, "secret-key") {
		t.Fatalf("secret leaked into plain settings: %s", raw)
	}
	if raw := s.GetSetting("rotation_group_secrets", ""); !strings.HasPrefix(raw, "ENC1") || strings.Contains(raw, "secret-key") {
		t.Fatalf("secret payload was not encrypted: %q", raw)
	}
	loaded, err := s.LoadRotationGroups()
	if err != nil || len(loaded) != 1 || loaded[0].DNS.DNSPodSecretKey != "secret-key" {
		t.Fatalf("load groups: %#v %v", loaded, err)
	}
	loaded[0].Name = "新名称"
	loaded[0].DNS.DNSPodSecretKey = "********"
	if err := s.SaveRotationGroups(loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = s.LoadRotationGroups()
	if err != nil || loaded[0].Name != "新名称" || loaded[0].DNS.DNSPodSecretKey != "secret-key" {
		t.Fatalf("masked secret was not preserved: %#v %v", loaded, err)
	}
}

func TestFailedTaskWithCreatedInstanceAllowsOneTimeCredentialRecovery(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask("task-partial", "preview", "group", "cn-test", "ecs.test", map[string]any{"loginPassword": "Password123!"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask("task-partial", map[string]any{"status": "failed", "instance_id": "i-created", "login_user": "root"}); err != nil {
		t.Fatal(err)
	}
	available, err := s.CredentialTaskInstances()
	if err != nil || !available["i-created"] {
		t.Fatalf("credential availability missing: %#v err=%v", available, err)
	}
	first, err := s.ConsumeInstanceTaskPassword("i-created")
	if err != nil || first.LoginPassword != "Password123!" || first.LoginUser != "root" {
		t.Fatalf("first credential recovery: task=%#v err=%v", first, err)
	}
	second, err := s.ConsumeInstanceTaskPassword("i-created")
	if err == nil || second != nil {
		t.Fatalf("credential was not one-time: task=%#v err=%v", second, err)
	}
}

func stringIndex(value, needle string) int {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
