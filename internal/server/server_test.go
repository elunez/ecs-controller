package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
)

func TestTemplateAndStaticAssetsUseCompressionAndCacheHeaders(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	root := t.TempDir()
	templatePath := filepath.Join(root, "template.html")
	if err := os.WriteFile(templatePath, []byte("<!doctype html><link href=\"static/app.css?v=__ASSET_VERSION__\">"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "static"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "static", "app.css"), []byte("body { color: #123; }"), 0600); err != nil {
		t.Fatal(err)
	}

	handler := New(st, t.TempDir(), templatePath).Handler()
	for _, test := range []struct {
		path     string
		cache    string
		wantBody string
		noToken  bool
	}{
		{path: "/", cache: "no-cache", wantBody: "static/app.css?v=", noToken: true},
		{path: "/static/app.css", cache: "public, max-age=31536000, immutable", wantBody: "body { color: #123; }"},
		{path: "/healthz", cache: "", wantBody: "\"ok\":true"},
	} {
		t.Run(test.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", response.StatusCode)
			}
			if got := response.Header.Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding=%q, want gzip", got)
			}
			if test.cache != "" && response.Header.Get("Cache-Control") != test.cache {
				t.Fatalf("Cache-Control=%q, want %q", response.Header.Get("Cache-Control"), test.cache)
			}
			reader, err := gzip.NewReader(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), test.wantBody) {
				t.Fatalf("response body %q does not contain %q", body, test.wantBody)
			}
			if test.noToken && strings.Contains(string(body), "__ASSET_VERSION__") {
				t.Fatal("template did not replace asset version token")
			}
		})
	}
}

func TestReleaseHasAssetsRequiresPackageChecksumAndSignature(t *testing.T) {
	var release githubRelease
	release.Assets = append(release.Assets, struct {
		Name string `json:"name"`
	}{Name: "ecs-controller-linux-amd64.tar.gz"})
	if releaseHasAssets(release, "ecs-controller-linux-amd64.tar.gz") {
		t.Fatal("release without checksums.txt must not be installable")
	}
	release.Assets = append(release.Assets, struct {
		Name string `json:"name"`
	}{Name: "checksums.txt"})
	if releaseHasAssets(release, "ecs-controller-linux-amd64.tar.gz") {
		t.Fatal("release without checksums.txt.sig must not be installable")
	}
	release.Assets = append(release.Assets, struct {
		Name string `json:"name"`
	}{Name: "checksums.txt.sig"})
	if !releaseHasAssets(release, "ecs-controller-linux-amd64.tar.gz") {
		t.Fatal("release package, checksum and signature should be installable")
	}
}

func TestCheckForUpdateUsesGitHubReleaseTagsAsDisplayVersions(t *testing.T) {
	currentCommit := strings.Repeat("a", 40)
	latestCommit := strings.Repeat("b", 40)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/elunez/ecs-controller/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.6.40",
				"html_url": "https://github.com/elunez/ecs-controller/releases/tag/v1.6.40",
				"body":     "## 更新内容\n\n- **展示版本更新说明** ([abc1234](https://github.com/elunez/ecs-controller/commit/abc1234))",
				"assets": []map[string]any{
					{"name": "ecs-controller-linux-amd64.tar.gz"},
					{"name": "checksums.txt"},
					{"name": "checksums.txt.sig"},
				},
			})
		case "/repos/elunez/ecs-controller/commits/v1.6.40":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":      latestCommit,
				"commit":   map[string]any{"message": "release build"},
				"html_url": "https://github.com/elunez/ecs-controller/commit/" + latestCommit,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "")
	srv.githubAPIBase = api.URL
	srv.packageChecker = func(githubRelease, string) bool { return true }

	previousCommit, previousVersion := app.Commit, app.Version
	app.Commit, app.Version = currentCommit, "v1.6.39"
	defer func() { app.Commit, app.Version = previousCommit, previousVersion }()

	recorder := httptest.NewRecorder()
	srv.checkForUpdate(recorder, httptest.NewRequest(http.MethodGet, "/index.php?action=check_update", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if got := result["current_version"]; got != "v1.6.39" {
		t.Fatalf("current_version=%q, want v1.6.39", got)
	}
	if got := result["repository"]; got != "elunez/ecs-controller" {
		t.Fatalf("repository=%q, want elunez/ecs-controller", got)
	}
	latest, ok := result["latest"].(map[string]any)
	if !ok || latest["version"] != "v1.6.40" {
		t.Fatalf("latest=%#v, want v1.6.40", result["latest"])
	}
	notes, ok := latest["notes"].([]any)
	if !ok || len(notes) != 1 || notes[0] != "展示版本更新说明" {
		t.Fatalf("notes=%#v, want release summary", latest["notes"])
	}
}

func TestSetupLoginAndCSRF(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dataDir := t.TempDir()
	templatePath := filepath.Join(t.TempDir(), "template.html")
	if err := os.WriteFile(templatePath, []byte("<!doctype html>ok"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := New(st, dataDir, templatePath)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	for _, path := range []string{"/index.php", "/index.php?action=view"} {
		resp, err := client.Get(httpSrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("legacy page route %s status: %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_password": "correct horse battery staple", "traffic_threshold": 95}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("missing username status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if st.IsInitialized() {
		t.Fatal("setup without username initialized the system")
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_username": "invalid username", "admin_password": "correct horse battery staple", "traffic_threshold": 95}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("invalid username status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_username": "admin.test", "admin_password": "correct horse battery staple", "traffic_threshold": 95}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup status: %d", resp.StatusCode)
	}
	csrf := resp.Header.Get("X-CSRF-Token")
	resp.Body.Close()
	if csrf == "" {
		t.Fatal("setup did not return csrf token")
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_username": "other.admin", "admin_password": "another secure password"}, nil)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("repeated setup status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"traffic_threshold": 90, "api_interval": 600}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf status: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"traffic_threshold": 90, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("valid csrf status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp, err = client.Get(httpSrv.URL + "/index.php?action=check_login")
	if err != nil {
		t.Fatal(err)
	}
	var check map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&check)
	resp.Body.Close()
	if check["logged_in"] != true {
		t.Fatal("session was not established")
	}
	if got := resp.Header.Get("X-CSRF-Token"); got != csrf {
		t.Fatalf("check_login did not restore csrf token: got %q want %q", got, csrf)
	}

	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("logo", "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	// Minimal 1x1 PNG. The endpoint validates the detected MIME type before saving.
	if _, err := part.Write([]byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/index.php?action=upload_logo", &upload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("logo upload status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if _, err := os.Stat(dataDir + "/brand-logo.png"); err != nil {
		t.Fatalf("logo was not saved: %v", err)
	}
}

func TestPasswordLoginRequiresMatchingUsernameAndPassword(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetAdminCredentials("admin.test", "secret1"); err != nil {
		t.Fatal(err)
	}

	srv := New(st, t.TempDir(), "")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{}

	for _, credentials := range []map[string]any{
		{"password": "secret1"},
		{"username": "wrong.user", "password": "secret1"},
		{"username": "admin.test", "password": "wrong1"},
	} {
		resp := postJSON(t, client, httpSrv.URL+"/index.php?action=login", credentials, nil)
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("invalid login status: %d body=%s", resp.StatusCode, body)
		}
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if result["success"] != false || result["message"] != "用户名或密码错误" {
			t.Fatalf("unexpected invalid login response: %#v", result)
		}
	}

	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=login", map[string]any{"username": "admin.test", "password": "secret1"}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("valid login status: %d body=%s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if result["success"] != true {
		t.Fatalf("valid login failed: %#v", result)
	}
}

func TestAdminCredentialsCanBeChangedFromConfig(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_username": "admin", "admin_password": "start1"}, nil)
	csrf := resp.Header.Get("X-CSRF-Token")
	resp.Body.Close()
	if csrf == "" {
		t.Fatal("setup did not return csrf token")
	}

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"admin_username": "operator", "admin_password": "six123", "traffic_threshold": 95, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("password change status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if !st.CheckAdminPassword("six123") || st.CheckAdminPassword("start1") {
		t.Fatal("admin password was not changed")
	}
	if !st.CheckAdminCredentials("operator", "six123") || st.CheckAdminCredentials("admin", "six123") {
		t.Fatal("admin username was not changed")
	}

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"admin_password": "********", "traffic_threshold": 95, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("masked password save status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if !st.CheckAdminPassword("six123") {
		t.Fatal("masked password overwrote the current password")
	}

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"admin_password": "12345", "traffic_threshold": 95, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("short password status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPasswordLoginCanBeDisabledAfterPasskeyIsRegistered(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetAdminCredentials("admin", "start1"); err != nil {
		t.Fatal(err)
	}

	srv := New(st, t.TempDir(), "")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{}

	if err := srv.saveConfig(map[string]any{"password_login_enabled": false}); err == nil {
		t.Fatal("password login was disabled without a Passkey")
	}
	if got := st.GetSetting("password_login_enabled", ""); got != "" && got != "1" {
		t.Fatalf("failed disable changed the setting: %q", got)
	}

	if err := st.SavePasskeyCredential("test-credential", `{"id":"test-credential"}`); err != nil {
		t.Fatal(err)
	}
	if err := srv.saveConfig(map[string]any{"password_login_enabled": false}); err != nil {
		t.Fatalf("disable password login: %v", err)
	}
	if got := st.GetSetting("password_login_enabled", ""); got != "0" {
		t.Fatalf("password login setting=%q, want 0", got)
	}

	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=login", map[string]any{"username": "admin", "password": "start1"}, nil)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("disabled password login status: %d body=%s", resp.StatusCode, body)
	}
	var disabledResponse map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&disabledResponse); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	message, _ := disabledResponse["message"].(string)
	if !strings.Contains(message, "Passkey") {
		t.Fatalf("disabled password login message=%#v", disabledResponse)
	}

	if err := srv.saveConfig(map[string]any{"password_login_enabled": true}); err != nil {
		t.Fatalf("re-enable password login: %v", err)
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=login", map[string]any{"username": "admin", "password": "start1"}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("re-enabled password login status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPasskeyWebAuthnUsesForwardedBrowserOrigin(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "")

	req := httptest.NewRequest(http.MethodPost, "https://internal:8080/index.php?action=passkey_login_start", nil)
	req.Host = "internal:8080"
	req.Header.Set("X-Forwarded-Host", "console.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	wa, err := srv.passkeyWebAuthn(req)
	if err != nil {
		t.Fatal(err)
	}
	if wa.Config.RPID != "console.example.com" {
		t.Fatalf("RP ID=%q, want console.example.com", wa.Config.RPID)
	}
	if len(wa.Config.RPOrigins) != 1 || wa.Config.RPOrigins[0] != "https://console.example.com" {
		t.Fatalf("RP origins=%#v, want https://console.example.com", wa.Config.RPOrigins)
	}
}

func TestPasskeyUserUsesConfiguredAdminUsername(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetAdminCredentials("operator.name", "secure-password"); err != nil {
		t.Fatal(err)
	}

	user, err := New(st, t.TempDir(), "").passkeyUser()
	if err != nil {
		t.Fatal(err)
	}
	if got := user.WebAuthnName(); got != "operator.name" {
		t.Fatalf("Passkey username=%q, want operator.name", got)
	}
	if got := string(user.WebAuthnID()); got != passkeyUserHandle {
		t.Fatalf("Passkey user handle=%q, want %q", got, passkeyUserHandle)
	}
}

func TestNotificationSwitchesPersistAndReadBack(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "")
	config := map[string]any{
		"traffic_threshold":       95,
		"api_interval":            600,
		"inventory_sync_enabled":  false,
		"inventory_sync_interval": 21600,
		"Notification": map[string]any{
			"email_enabled": false,
			"email":         "ops@example.com",
			"daily_enabled": true,
			"daily_time":    "01:30",
			"telegram": map[string]any{
				"enabled":     true,
				"confirm_ttl": 60,
			},
			"webhook": map[string]any{
				"enabled": true,
			},
		},
	}
	if err := srv.saveConfig(config); err != nil {
		t.Fatal(err)
	}

	settings := st.Settings()
	if settings["notify_email_enabled"] != "0" || settings["notify_tg_enabled"] != "1" || settings["notify_wh_enabled"] != "1" {
		t.Fatalf("notification switches were not normalized: email=%q telegram=%q webhook=%q", settings["notify_email_enabled"], settings["notify_tg_enabled"], settings["notify_wh_enabled"])
	}
	if settings["notify_daily_enabled"] != "1" || settings["notify_daily_time"] != "01:30" {
		t.Fatalf("daily summary settings were not normalized: enabled=%q time=%q", settings["notify_daily_enabled"], settings["notify_daily_time"])
	}
	if settings["inventory_sync_enabled"] != "0" || settings["inventory_sync_interval"] != "21600" {
		t.Fatalf("inventory sync settings were not saved: enabled=%q interval=%q", settings["inventory_sync_enabled"], settings["inventory_sync_interval"])
	}
	readBack := notificationSettings(settings)
	telegram := readBack["telegram"].(map[string]any)
	webhook := readBack["webhook"].(map[string]any)
	if readBack["email_enabled"] != false || readBack["daily_enabled"] != true || readBack["daily_time"] != "01:30" || telegram["enabled"] != true || webhook["enabled"] != true {
		t.Fatalf("notification switches did not read back: %#v", readBack)
	}

	legacy := notificationSettings(map[string]string{
		"notify_email_enabled": "false",
		"notify_tg_enabled":    "true",
		"notify_wh_enabled":    "false",
	})
	legacyTelegram := legacy["telegram"].(map[string]any)
	legacyWebhook := legacy["webhook"].(map[string]any)
	if legacy["email_enabled"] != false || legacyTelegram["enabled"] != true || legacyWebhook["enabled"] != false {
		t.Fatalf("legacy notification switches were not interpreted: %#v", legacy)
	}
}

func TestSaveConfigRejectsDuplicateAccountRegion(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "")
	err = srv.saveConfig(map[string]any{
		"Accounts": []any{
			map[string]any{"AccessKeyId": "ak", "AccessKeySecret": "sk", "regionId": "cn-hongkong"},
			map[string]any{"AccessKeyId": "ak", "AccessKeySecret": "sk", "regionId": "CN-HONGKONG"},
		},
	})
	if err == nil {
		t.Fatal("saveConfig accepted duplicate account and region")
	}
	if got := st.GetSetting("traffic_threshold", ""); got != "" {
		t.Fatalf("duplicate configuration partially changed settings: %q", got)
	}
}

func TestSaveConfigSyncsGroupWithGeneratedKey(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "")
	var receivedAccount app.Account
	srv.CloudFactory = func(account app.Account) cloud.Client {
		receivedAccount = account
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-generated", Status: "Running"}}}
	}
	if err := srv.saveConfig(map[string]any{
		"Accounts": []any{map[string]any{
			"AccessKeyId":     "ak",
			"AccessKeySecret": "sk",
			"regionId":        "cn-test",
			"maxTraffic":      200,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	groups, err := st.LoadGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].GroupKey == "" {
		t.Fatalf("generated group key was not persisted: %#v", groups)
	}
	if receivedAccount.AccessKeyID != "ak" || receivedAccount.AccessKeySecret != "sk" || receivedAccount.RegionID != "cn-test" {
		t.Fatalf("sync did not use the saved account credentials: %#v", receivedAccount)
	}
	for _, log := range st.Logs("", 20) {
		if log["message"] == "账号组同步失败: 账号组不存在" {
			t.Fatalf("sync used the pre-save empty group key: %#v", log)
		}
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].GroupKey != groups[0].GroupKey {
		t.Fatalf("synced account was not linked to the generated group: %#v", accounts)
	}
}

func TestTestAccountResolvesMaskedSecret(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong"}}); err != nil {
		t.Fatal(err)
	}

	srv := New(st, t.TempDir(), "")
	secret, err := srv.resolveMaskedAccountSecret(map[string]any{"groupKey": "group-1"}, "ak", "cn-hongkong")
	if err != nil || secret != "sk" {
		t.Fatalf("masked account secret was not restored: secret=%q err=%v", secret, err)
	}
	if _, err := srv.resolveMaskedAccountSecret(map[string]any{"groupKey": "group-1"}, "other-ak", "cn-hongkong"); err == nil {
		t.Fatal("secret was restored for a different access key")
	}
}

func TestTaskResponsePreservesFrontendCredentialFields(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateTask("task-1", "preview-1", "group-1", "cn-test", "ecs.test", map[string]any{"loginPassword": "Password123!"}); err != nil {
		t.Fatal(err)
	}
	publicTask, err := st.GetTask("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if publicTask.LoginPassword != "" {
		t.Fatal("queued task exposed its password")
	}
	task, err := st.GetTaskForWorker("task-1")
	if err != nil {
		t.Fatal(err)
	}
	response := taskResponse(task)
	if response["loginPassword"] != "Password123!" {
		t.Fatalf("camelCase password missing: %#v", response)
	}
	if response["task_id"] != "task-1" || response["taskId"] != "task-1" {
		t.Fatalf("task aliases missing: %#v", response)
	}
	if err := st.UpdateTask("task-1", map[string]any{"status": "success"}); err != nil {
		t.Fatal(err)
	}
	first, err := st.ConsumeTaskPassword("task-1")
	if err != nil || first.LoginPassword != "Password123!" {
		t.Fatalf("first credential read: task=%+v err=%v", first, err)
	}
	second, err := st.ConsumeTaskPassword("task-1")
	if err != nil || second.LoginPassword != "" {
		t.Fatalf("credential was not one-time: task=%+v err=%v", second, err)
	}
}

func TestFailedCreateTaskWithInstanceReturnsCredentialOnce(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateTask("task-failed", "preview", "group", "cn-test", "ecs.test", map[string]any{"loginPassword": "Password123!"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTask("task-failed", map[string]any{"status": "failed", "instance_id": "i-created", "login_user": "root", "error_message": "post-create failure"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	recorder := httptest.NewRecorder()
	srv.task(recorder, httptest.NewRequest(http.MethodGet, "/index.php?action=get_ecs_create_task&taskId=task-failed", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("task status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data, _ := response["data"].(map[string]any)
	if data["instanceId"] != "i-created" || data["loginPassword"] != "Password123!" {
		t.Fatalf("failed task credential missing: %#v", response)
	}
	recorder = httptest.NewRecorder()
	srv.task(recorder, httptest.NewRequest(http.MethodGet, "/index.php?action=get_ecs_create_task&taskId=task-failed", nil))
	response = map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data, _ = response["data"].(map[string]any)
	if data["loginPassword"] != "" {
		t.Fatalf("failed task credential was returned twice: %#v", response)
	}
}

func TestConfigDoesNotDoubleCountCDTAggregateAcrossInstances(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	for _, instanceID := range []string{"i-1", "i-2"} {
		if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: instanceID, TrafficUsed: 12.5, TrafficAPIStatus: "fallback_cdt", TrafficAPIMessage: "CDT aggregate", UpdatedAt: 100}); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(st, t.TempDir(), "")
	recorder := httptest.NewRecorder()
	srv.config(recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("config status: %d", recorder.Code)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	items, ok := response["Accounts"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected account groups: %#v", response["Accounts"])
	}
	item := items[0].(map[string]any)
	if item["usageUsed"] != 12.5 {
		t.Fatalf("CDT aggregate was double-counted: %#v", item["usageUsed"])
	}
}

type fakePreflightClient struct {
	cloud.Client
	priceRequest cloud.CreatePriceRequest
	priceErr     error
	customImages []map[string]any
	diskImageID  string
	diskZoneID   string
}

type fakeStopClient struct {
	cloud.Client
	mode string
}

func (f *fakeStopClient) StopInstance(_ context.Context, _, _, mode string) error {
	f.mode = mode
	return nil
}

func (f *fakePreflightClient) DescribeInstanceType(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"InstanceTypeId": "ecs.test", "CpuArchitecture": "X86"}, nil
}

func (f *fakePreflightClient) DescribeAvailableZones(context.Context, string, string, string, string) ([]map[string]any, error) {
	return []map[string]any{{"ZoneId": "zone-a", "Status": "Available"}, {"ZoneId": "zone-b", "Status": "Available"}}, nil
}

func (f *fakePreflightClient) DescribeImagesForArchitecture(context.Context, string, string, string) ([]map[string]any, error) {
	return []map[string]any{
		{"ImageId": "img-x86", "ImageName": "win2022_uefi_x64", "OSName": "Windows Server 2022", "OSType": "windows", "Architecture": "x86_64", "Size": 20},
		{"ImageId": "img-unlisted", "ImageName": "fedora_42_x64", "OSName": "Fedora 42", "OSType": "linux", "Architecture": "x86_64", "Size": 20},
	}, nil
}

func (f *fakePreflightClient) DescribeCustomImages(context.Context, string, string) ([]map[string]any, error) {
	if f.customImages != nil {
		return f.customImages, nil
	}
	return []map[string]any{{"ImageId": "img-custom", "ImageName": "alpine-3.23", "OSName": "Alpine Linux", "OSType": "linux", "Architecture": "x86_64", "Size": 5}}, nil
}

func (f *fakePreflightClient) GetSystemDiskOptions(_ context.Context, _, zoneID, _, _, imageID string) ([]map[string]any, error) {
	f.diskImageID = imageID
	f.diskZoneID = zoneID
	return []map[string]any{{"value": "cloud_essd", "label": "ESSD", "min": 40, "max": 100, "unit": "GB"}}, nil
}

func (f *fakePreflightClient) EstimateCreatePrice(_ context.Context, request cloud.CreatePriceRequest) (cloud.CreatePriceEstimate, error) {
	f.priceRequest = request
	if f.priceErr != nil {
		return cloud.CreatePriceEstimate{}, f.priceErr
	}
	return cloud.CreatePriceEstimate{Available: true, Currency: "CNY", HourlyPrice: 0.12, DailyPrice: 2.88, MonthlyPrice: 86.4, OriginalHourly: 0.15, DiscountHourly: 0.03, Components: []cloud.PriceComponent{{Resource: "instanceType", Label: "ECS 计算资源", OriginalPrice: 0.1, TradePrice: 0.08}, {Resource: "systemDisk", Label: "系统盘", OriginalPrice: 0.05, DiscountPrice: 0.01, TradePrice: 0.04}}, PublicNetworkNote: "公网流量按实际使用量计费，不包含在固定资源预估中。", Source: "Alibaba Cloud ECS DescribePrice"}, nil
}

func TestECSImageCloudInitUserData(t *testing.T) {
	tests := []struct {
		name       string
		image      map[string]any
		wantSystem string
		wantConfig string
	}{
		{name: "Debian", image: map[string]any{"OSName": "Debian 12"}, wantSystem: "Debian", wantConfig: "systemctl try-restart ssh.service"},
		{name: "Alpine", image: map[string]any{"ImageName": "alpine-3.23.2-x86_64"}, wantSystem: "Alpine", wantConfig: "rc-service sshd restart"},
		{name: "unsupported", image: map[string]any{"OSName": "Ubuntu 24.04"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userData, systemName := ecsImageCloudInitUserData(tt.image)
			if systemName != tt.wantSystem {
				t.Fatalf("system = %q, want %q", systemName, tt.wantSystem)
			}
			if tt.wantConfig == "" {
				if userData != "" {
					t.Fatalf("unsupported image unexpectedly received cloud-init: %q", userData)
				}
				return
			}
			if !strings.Contains(userData, "PasswordAuthentication yes") || !strings.Contains(userData, "PermitRootLogin yes") || !strings.Contains(userData, tt.wantConfig) {
				t.Fatalf("cloud-init config missing required settings: %q", userData)
			}
		})
	}
}

func TestManualStopUsesInstanceShutdownMode(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: "i-1", InstanceStatus: "Running", StoppedMode: "StopCharging"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %#v %v", accounts, err)
	}
	fake := &fakeStopClient{}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	recorder := httptest.NewRecorder()
	srv.control(recorder, map[string]any{"accountId": accounts[0].ID, "action": "stop", "shutdownMode": "KeepCharging"})
	if recorder.Code != http.StatusOK || fake.mode != "StopCharging" {
		t.Fatalf("manual stop mode=%q status=%d body=%s", fake.mode, recorder.Code, recorder.Body.String())
	}
}

type fakeRefreshClient struct {
	cloud.Client
	instance      cloud.Instance
	describeErr   error
	monthlyBytes  float64
	monthlyPoints int
	monthlyErr    error
	monthlyCalls  int
	deltaBytes    float64
	deltaLastMS   int64
	deltaPoints   int
	deltaErr      error
	deltaCalls    int
}

func (f *fakeRefreshClient) DescribeInstance(context.Context, string, string) (*cloud.Instance, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	instance := f.instance
	return &instance, nil
}

func TestRefreshAccountHidesInstanceConfirmedMissingFromCloud(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-missing", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeRefreshClient{describeErr: &cloud.APIError{Code: "InvalidInstanceId.NotFound", HTTPStatus: 404}}
	}
	recorder := httptest.NewRecorder()
	srv.refreshAccount(recorder, map[string]any{"id": accounts[0].ID})
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"removed":true`) {
		t.Fatalf("refresh response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := st.Account(accounts[0].ID, false); err == nil {
		t.Fatal("cloud-missing instance remained visible")
	}
	hidden, err := st.Account(accounts[0].ID, true)
	if err != nil || hidden.IsDeleted != 1 || hidden.InstanceStatus != "Releasing" {
		t.Fatalf("cloud-missing instance was not queued safely: %#v %v", hidden, err)
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job == nil || job.Kind != "delete_instance" || job.EntityKey != fmt.Sprint(accounts[0].ID) {
		t.Fatalf("missing cleanup job: %#v %v", job, err)
	}
}

func (f *fakeRefreshClient) GetInstanceMonthlyTraffic(context.Context, string, string, string, int64, int64) (float64, int, error) {
	f.monthlyCalls++
	return f.monthlyBytes, f.monthlyPoints, f.monthlyErr
}

func (f *fakeRefreshClient) GetOutboundTrafficDelta(context.Context, string, string, string, int64, int64) (float64, int64, int, string, error) {
	f.deltaCalls++
	return f.deltaBytes, f.deltaLastMS, f.deltaPoints, "InternetOutRate", f.deltaErr
}

func TestRefreshAccountUsesMonthlyCMSValueBeforeDelta(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertAccount(app.Account{
		ID:              1,
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		RegionID:        "cn-hongkong",
		InstanceID:      "i-1",
		PublicIP:        "203.0.113.10",
		TrafficUsed:     0.0008,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	month := now.Format("2006-01")
	if _, err := st.SetInstanceTraffic(1, "i-1", month, 0.0008*1024*1024*1024, now.Add(-5*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	fake := &fakeRefreshClient{
		instance:      cloud.Instance{ID: "i-1", Status: "Running", PublicIP: "203.0.113.10"},
		monthlyBytes:  8 * 1024 * 1024 * 1024,
		monthlyPoints: 1,
		deltaBytes:    10 * 1024 * 1024,
		deltaLastMS:   now.UnixMilli(),
		deltaPoints:   1,
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	recorder := httptest.NewRecorder()
	srv.refreshAccount(recorder, map[string]any{"id": 1})
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fake.monthlyCalls != 1 || fake.deltaCalls != 0 {
		t.Fatalf("unexpected CMS calls: monthly=%d delta=%d", fake.monthlyCalls, fake.deltaCalls)
	}

	sample, err := st.InstanceTrafficUsage(1, "i-1", month)
	if err != nil {
		t.Fatal(err)
	}
	if sample.TrafficBytes != 8*1024*1024*1024 {
		t.Fatalf("monthly CMS value was not stored absolutely: got=%v", sample.TrafficBytes)
	}
	account, err := st.Account(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if account.TrafficUsed != 8 || account.TrafficBillingMonth != month || account.TrafficAPIStatus != "ok" {
		t.Fatalf("account traffic state=%+v", account)
	}
}

func TestRefreshAccountFallsBackToDeltaWhenMonthlyCMSHasNoPoints(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertAccount(app.Account{ID: 1, AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: "i-1"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	month := now.Format("2006-01")
	fake := &fakeRefreshClient{
		instance:      cloud.Instance{ID: "i-1", Status: "Running"},
		monthlyPoints: 0,
		deltaBytes:    12 * 1024 * 1024,
		deltaLastMS:   now.UnixMilli(),
		deltaPoints:   1,
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	recorder := httptest.NewRecorder()
	srv.refreshAccount(recorder, map[string]any{"id": 1})
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fake.monthlyCalls != 1 || fake.deltaCalls != 1 {
		t.Fatalf("monthly no-data did not fall back to delta: monthly=%d delta=%d", fake.monthlyCalls, fake.deltaCalls)
	}
	sample, err := st.InstanceTrafficUsage(1, "i-1", month)
	if err != nil {
		t.Fatal(err)
	}
	if sample.TrafficBytes != 12*1024*1024 {
		t.Fatalf("delta fallback was not stored: got=%v", sample.TrafficBytes)
	}
}

type fakeSyncClient struct {
	cloud.Client
	instances        []cloud.Instance
	describeErr      error
	publicNetworks   map[string]cloud.InstancePublicNetwork
	publicNetworkErr error
}

type blockingSyncClient struct {
	*fakeSyncClient
	key     string
	entered chan<- string
	release <-chan struct{}
}

func (f *blockingSyncClient) DescribeInstances(ctx context.Context, _ string) ([]cloud.Instance, error) {
	f.entered <- f.key
	select {
	case <-f.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeSyncClient) DescribeInstances(context.Context, string) ([]cloud.Instance, error) {
	return f.instances, f.describeErr
}

func (f *fakeSyncClient) DescribeInstancePublicNetworks(context.Context, string, []string) (map[string]cloud.InstancePublicNetwork, error) {
	return f.publicNetworks, f.publicNetworkErr
}

func TestSyncInventoryRunsAccountGroupsConcurrently(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	groups := []app.AccountGroup{
		{GroupKey: "group-a", AccessKeyID: "ak-a", AccessKeySecret: "sk", RegionID: "cn-a"},
		{GroupKey: "group-b", AccessKeyID: "ak-b", AccessKeySecret: "sk", RegionID: "cn-b"},
	}
	if err := st.SaveGroups(groups); err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 2)
	release := make(chan struct{})
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(account app.Account) cloud.Client {
		return &blockingSyncClient{fakeSyncClient: &fakeSyncClient{}, key: account.AccessKeyID, entered: entered, release: release}
	}
	done := make(chan error, 1)
	go func() {
		_, syncErr := srv.SyncInventory(context.Background())
		done <- syncErr
	}()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case key := <-entered:
			seen[key] = true
		case <-time.After(time.Second):
			t.Fatalf("inventory groups did not run concurrently: %#v", seen)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type fakeBillingDetailsClient struct {
	cloud.Client
	details []cloud.BillingDetail
	calls   int
}

func (f *fakeBillingDetailsClient) GetBillingDetails(context.Context, string, string, string) ([]cloud.BillingDetail, error) {
	f.calls++
	return f.details, nil
}

func TestBillDetailsFiltersRecentItemsAndCachesResult(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting("enable_billing", "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-billing", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", SiteType: "china"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{ID: 7, AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", SiteType: "china", GroupKey: "group-billing", InstanceID: "i-1"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	fake := &fakeBillingDetailsClient{details: []cloud.BillingDetail{
		{Date: now.Format("2006-01-02"), ProductName: "云服务器 ECS", Amount: 1.25, Currency: "CNY"},
		{Date: now.AddDate(0, 0, -20).Format("2006-01-02"), ProductName: "云盘", Amount: 0.10, Currency: "CNY"},
	}}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	body := strings.NewReader(`{"group_key":"group-billing","days":1}`)
	req := httptest.NewRequest(http.MethodPost, "/index.php?action=get_bill_details", body)
	recorder := httptest.NewRecorder()
	srv.billDetails(recorder, req, map[string]any{"group_key": "group-billing", "days": 1})
	if recorder.Code != http.StatusOK {
		t.Fatalf("bill details status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data struct {
			Items []cloud.BillingDetail `json:"items"`
			Total float64               `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Items) != 1 || response.Data.Items[0].ProductName != "云服务器 ECS" || response.Data.Total != 1.25 {
		t.Fatalf("unexpected filtered details: %#v", response.Data)
	}

	recorder = httptest.NewRecorder()
	srv.billDetails(recorder, req, map[string]any{"group_key": "group-billing", "days": 1})
	if recorder.Code != http.StatusOK || fake.calls != 1 {
		t.Fatalf("cached bill details status=%d calls=%d", recorder.Code, fake.calls)
	}
}

func TestBillingDatesIncludesPreviousMonth(t *testing.T) {
	dates := billingDates("2026-07-27", "2026-08-02")
	if len(dates) != 7 || dates[0] != "2026-07-27" || dates[6] != "2026-08-02" {
		t.Fatalf("unexpected billing dates: %#v", dates)
	}
}

func TestEnrichBillingDetailsAddsCurrentResourceWithoutChangingBillValues(t *testing.T) {
	items := []cloud.BillingDetail{{
		InstanceID: "eip-1",
		Usage:      22,
		Amount:     0.01,
	}}
	enrichBillingDetails(items, map[string]cloud.BillingResource{
		"eip-1": {
			InstanceID: "i-1",
			EIP: &cloud.BillingEIP{
				AllocationID: "eip-1",
				Status:       "InUse",
				Bandwidth:    200,
				Count:        1,
			},
		},
	})

	if items[0].CurrentResource == nil || items[0].CurrentResource.EIP == nil || items[0].CurrentResource.EIP.Bandwidth != 200 {
		t.Fatalf("current resource not attached: %#v", items[0])
	}
	if items[0].Usage != 22 || items[0].Amount != 0.01 {
		t.Fatalf("billing values changed during enrichment: %#v", items[0])
	}
}

func TestSyncGroupPreservesReleaseAndQueuesMissingInstances(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-release", InstanceStatus: "Releasing"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-missing", InstanceStatus: "Stopped"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-release", Status: "Running", PublicIP: "203.0.113.20"}}}
	}
	count, err := srv.syncGroup("group-1")
	if err != nil || count != 1 {
		t.Fatalf("sync result: %d %v", count, err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	var release *app.Account
	for i := range accounts {
		switch accounts[i].InstanceID {
		case "i-release":
			release = &accounts[i]
		}
	}
	if release == nil || release.InstanceStatus != "Releasing" {
		t.Fatalf("release state was resurrected: %#v", release)
	}
	all, err := st.LoadAccounts(true)
	if err != nil {
		t.Fatal(err)
	}
	var missing *app.Account
	for i := range all {
		if all[i].InstanceID == "i-missing" {
			missing = &all[i]
		}
	}
	if missing == nil || missing.InstanceStatus != "Releasing" || missing.IsDeleted != 1 {
		t.Fatalf("missing instance was not hidden and queued: %#v", missing)
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job == nil || job.EntityKey != fmt.Sprint(missing.ID) {
		t.Fatalf("missing cleanup job: %#v %v", job, err)
	}
}

func TestAutomaticInventoryRequiresTwoConsecutiveMissingObservations(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-auto", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-auto", InstanceID: "i-missing", InstanceStatus: "Stopped"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v %v", accounts, err)
	}
	id := accounts[0].ID
	srv := New(st, t.TempDir(), "")
	fake := &fakeSyncClient{}
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }

	if count, err := srv.SyncInventory(context.Background()); err != nil || count != 0 {
		t.Fatalf("first inventory sync: count=%d err=%v", count, err)
	}
	account, err := st.Account(id, false)
	if err != nil || account.InstanceStatus != "Stopped" {
		t.Fatalf("first missing observation changed account: %#v %v", account, err)
	}
	if job, err := st.ClaimJob(time.Minute); err != nil || job != nil {
		t.Fatalf("first missing observation queued cleanup: %#v %v", job, err)
	}

	fake.instances = []cloud.Instance{{ID: "i-missing", Status: "Stopped"}}
	if count, err := srv.SyncInventory(context.Background()); err != nil || count != 1 {
		t.Fatalf("reappeared inventory sync: count=%d err=%v", count, err)
	}
	fake.instances = nil
	if count, err := srv.SyncInventory(context.Background()); err != nil || count != 0 {
		t.Fatalf("missing inventory sync after reset: count=%d err=%v", count, err)
	}
	account, err = st.Account(id, false)
	if err != nil || account.InstanceStatus != "Stopped" {
		t.Fatalf("missing counter was not reset when instance returned: %#v %v", account, err)
	}
	if job, err := st.ClaimJob(time.Minute); err != nil || job != nil {
		t.Fatalf("first missing observation after reset queued cleanup: %#v %v", job, err)
	}

	if count, err := srv.SyncInventory(context.Background()); err != nil || count != 0 {
		t.Fatalf("second consecutive missing inventory sync: count=%d err=%v", count, err)
	}
	if account, err = st.Account(id, false); err == nil {
		t.Fatalf("second missing observation remained visible: %#v", account)
	}
	account, err = st.Account(id, true)
	if err != nil || account.InstanceStatus != "Releasing" || account.IsDeleted != 1 {
		t.Fatalf("second missing observation did not hide and queue cleanup: %#v %v", account, err)
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job == nil || job.EntityKey != fmt.Sprint(id) {
		t.Fatalf("missing cleanup job: %#v %v", job, err)
	}
}

func TestSyncGroupRemovesCloudMissingReleaseFailedRecords(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-old", PublicIP: "203.0.113.10", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-current", Status: "Running", PublicIP: "203.0.113.10"}}}
	}
	if count, err := srv.syncGroup("group-1"); err != nil || count != 1 {
		t.Fatalf("sync result: count=%d err=%v", count, err)
	}
	visible, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].InstanceID != "i-current" {
		t.Fatalf("unexpected visible accounts: %#v", visible)
	}
	all, err := st.LoadAccounts(true)
	if err != nil {
		t.Fatal(err)
	}
	var old *app.Account
	for i := range all {
		if all[i].InstanceID == "i-old" {
			old = &all[i]
		}
	}
	if old == nil || old.IsDeleted != 2 || old.InstanceStatus != "Released" {
		t.Fatalf("release-failed orphan was not retired: %#v", old)
	}
	if job, err := st.ClaimJob(time.Second); err != nil || job != nil {
		t.Fatalf("orphan cleanup unexpectedly queued another release job: job=%#v err=%v", job, err)
	}
}

func TestSyncGroupKeepsReleaseFailedRecordWhenCloudSyncFails(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-old", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{describeErr: fmt.Errorf("temporary DescribeInstances failure")}
	}
	if _, err := srv.syncGroup("group-1"); err == nil {
		t.Fatal("sync unexpectedly succeeded")
	}
	account, err := st.Account(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if account.InstanceStatus != "ReleaseFailed" {
		t.Fatalf("failed cloud sync changed local release state: %#v", account)
	}
}

func TestSyncGroupKeepsReleaseFailedRecordWhenInstanceStillExists(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-existing", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-existing", Status: "Running"}}}
	}
	if count, err := srv.syncGroup("group-1"); err != nil || count != 1 {
		t.Fatalf("sync result: count=%d err=%v", count, err)
	}
	account, err := st.Account(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if account.InstanceID != "i-existing" || account.InstanceStatus != "Running" {
		t.Fatalf("existing cloud instance was not retained and refreshed: %#v", account)
	}
}

func TestSyncGroupRefreshesBoundEIPBandwidth(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	group := app.AccountGroup{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", MaxTraffic: 200}
	if err := st.SaveGroups([]app.AccountGroup{group}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "group-1", InstanceID: "i-1", InternetBandwidth: 10, PublicIPMode: "ecs_public_ip"}); err != nil {
		t.Fatal(err)
	}

	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{
			instances: []cloud.Instance{{ID: "i-1", Status: "Running", PublicIP: "203.0.113.20"}},
			publicNetworks: map[string]cloud.InstancePublicNetwork{
				"i-1": {AllocationID: "eip-1", Address: "203.0.113.20", Bandwidth: 200},
			},
		}
	}
	if count, err := srv.syncGroup("group-1"); err != nil || count != 1 {
		t.Fatalf("sync result: count=%d err=%v", count, err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts=%#v", accounts)
	}
	account := accounts[0]
	if account.InternetBandwidth != 200 || account.PublicIPMode != "eip" || account.EIPAllocationID != "eip-1" || account.PublicIP != "203.0.113.20" {
		t.Fatalf("EIP network was not refreshed: %#v", account)
	}
	if account.EIPManaged {
		t.Fatal("externally discovered EIP was incorrectly marked controller-managed")
	}
}

func TestPreviewUsesDynamicArchitectureZoneDiskAndWindowsPort(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", Remark: "test"}}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	fake := &fakePreflightClient{customImages: []map[string]any{{"ImageId": "img-custom", "ImageName": "alpine-3.23.2-x86_64", "OSName": "Alpine Linux", "OSType": "linux", "Architecture": "x86_64", "Size": 5}}}
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_username": "admin", "admin_password": "correct horse battery staple"}, nil)
	csrf := resp.Header.Get("X-CSRF-Token")
	resp.Body.Close()
	if csrf == "" {
		t.Fatal("setup did not return csrf token")
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=preview_ecs_create", map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test", "osKey": "windows_2022", "zoneId": "zone-a",
		"systemDiskCategory": "cloud_essd_entry", "systemDiskSize": 20, "publicIpMode": "ecs_public_ip", "billingMode": "spot", "clientCidrIp": "192.0.2.10/32",
	}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("preview status: %d body=%s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	summary, ok := result["summary"].(map[string]any)
	if !ok || summary["imageId"] != "img-x86" || summary["loginPort"] != float64(3389) || summary["zoneId"] != "zone-a" || summary["billingMode"] != "spot" || summary["billingModeLabel"] != "抢占式（市场价）" {
		t.Fatalf("dynamic preview fields missing: %#v", result)
	}
	windowsNetwork, _ := summary["network"].(map[string]any)
	windowsSecurityGroup, _ := windowsNetwork["securityGroup"].(map[string]any)
	if windowsSecurityGroup["cidr"] != "192.0.2.10/32" || !strings.Contains(fmt.Sprint(windowsSecurityGroup["rules"]), "TCP 3389 / 192.0.2.10/32") {
		t.Fatalf("windows preview did not retain restricted RDP access: %#v", windowsSecurityGroup)
	}
	disk, ok := summary["systemDisk"].(map[string]any)
	if !ok || disk["category"] != "cloud_essd" || disk["size"] != float64(40) || disk["min"] != float64(40) {
		t.Fatalf("dynamic disk fields missing: %#v", summary["systemDisk"])
	}
	pricing, ok := result["pricing"].(map[string]any)
	if !ok || pricing["available"] != true || pricing["hourlyPrice"] != 0.12 || pricing["monthlyPrice"] != 86.4 || pricing["source"] != "Alibaba Cloud ECS DescribePrice" {
		t.Fatalf("real-time pricing missing from preview: %#v", result["pricing"])
	}
	if fake.priceRequest.DiskCategory != "cloud_essd" || fake.priceRequest.DiskSize != 40 || fake.priceRequest.ZoneID != "zone-a" || fake.priceRequest.ImageID != "img-x86" || fake.priceRequest.BillingMode != "spot" {
		t.Fatalf("pricing did not use final preflight configuration: %#v", fake.priceRequest)
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=get_ecs_images", map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test",
	}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("image options status: %d body=%s", resp.StatusCode, body)
	}
	var imageOptions map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&imageOptions); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	imageData, _ := imageOptions["data"].(map[string]any)
	options, _ := imageData["options"].([]any)
	if len(options) != 2 || options[0].(map[string]any)["value"] != "img-x86" || options[0].(map[string]any)["label"] != "Windows Server 2022" || options[0].(map[string]any)["source"] != "system" || options[1].(map[string]any)["value"] != "img-custom" || options[1].(map[string]any)["source"] != "custom" || options[1].(map[string]any)["size"] != float64(5) {
		t.Fatalf("merged image options missing: %#v", imageOptions)
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=get_ecs_disk_options", map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test", "imageSource": "system", "imageId": "img-x86", "systemDiskCategory": "cloud_essd",
	}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("zone options status: %d body=%s", resp.StatusCode, body)
	}
	var zoneResponse map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&zoneResponse); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	zoneData, _ := zoneResponse["data"].(map[string]any)
	zoneOptions, _ := zoneData["zones"].([]any)
	diskOptions, _ := zoneData["options"].([]any)
	if len(zoneOptions) != 2 || zoneOptions[0].(map[string]any)["value"] != "zone-a" || zoneOptions[1].(map[string]any)["value"] != "zone-b" || len(diskOptions) != 0 {
		t.Fatalf("selectable zones missing or disk options loaded before selection: %#v", zoneResponse)
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=get_ecs_disk_options", map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test", "imageSource": "system", "imageId": "img-x86", "zoneId": "zone-b", "systemDiskCategory": "cloud_essd",
	}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("selected zone disk options status: %d body=%s", resp.StatusCode, body)
	}
	var selectedZoneResponse map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&selectedZoneResponse); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	selectedZoneData, _ := selectedZoneResponse["data"].(map[string]any)
	selectedDiskOptions, _ := selectedZoneData["options"].([]any)
	if selectedZoneData["zoneId"] != "zone-b" || len(selectedDiskOptions) == 0 || fake.diskZoneID != "zone-b" {
		t.Fatalf("selected zone was not used for disk options: %#v", selectedZoneResponse)
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=preview_ecs_create", map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test", "imageSource": "custom", "imageId": "img-custom", "zoneId": "zone-a",
		"systemDiskCategory": "cloud_essd", "systemDiskSize": 1, "publicIpMode": "ecs_public_ip", "clientCidrIp": "192.0.2.10/32",
	}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("custom image preview status: %d body=%s", resp.StatusCode, body)
	}
	var customPreview map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&customPreview); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	customSummary, _ := customPreview["summary"].(map[string]any)
	customDisk, _ := customSummary["systemDisk"].(map[string]any)
	if customSummary["imageId"] != "img-custom" || customSummary["imageSource"] != "custom" || customSummary["cloudInitEnabled"] != true || customSummary["loginUser"] != "root" || customDisk["min"] != float64(5) || customDisk["size"] != float64(5) || fake.diskImageID != "img-custom" {
		t.Fatalf("custom image preview missing: summary=%#v diskImageId=%q", customSummary, fake.diskImageID)
	}
	if !strings.Contains(fmt.Sprint(customPreview["warnings"]), "Alpine 自定义镜像将通过 cloud-init") {
		t.Fatalf("Alpine cloud-init warning missing: %#v", customPreview["warnings"])
	}
	fake.priceErr = errors.New("price service unavailable")
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=preview_ecs_create", map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test", "osKey": "windows_2022", "zoneId": "zone-a",
		"systemDiskCategory": "cloud_essd_entry", "systemDiskSize": 20, "publicIpMode": "ecs_public_ip", "clientCidrIp": "192.0.2.10/32",
	}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("pricing failure status: %d body=%s", resp.StatusCode, body)
	}
	var priceFailure map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&priceFailure); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(fmt.Sprint(priceFailure["error"]), "费用预估失败") {
		t.Fatalf("pricing failure was not returned: %#v", priceFailure)
	}
}

func TestPreviewAcceptsInstanceScheduleAndStripsRotationGroup(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", Remark: "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRotationGroups([]app.RotationGroup{{ID: "rotation-1", Name: "亚洲轮转", Domain: "service.example.com", Provider: "dnspod", DNS: app.DNSCredentials{DNSPodSecretID: "id", DNSPodSecretKey: "key", TTL: 600}}}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client { return &fakePreflightClient{} }
	data := map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test", "osKey": "debian_12", "zoneId": "zone-a",
		"systemDiskCategory": "cloud_essd_entry", "systemDiskSize": 40, "publicIpMode": "ecs_public_ip", "clientCidrIp": "192.0.2.10/32",
		"scheduleEnabled": true, "startTime": "08:00", "stopTime": "20:00", "stoppedMode": "StopCharging", "rotationGroupId": "rotation-1",
	}
	recorder := httptest.NewRecorder()
	srv.preview(recorder, httptest.NewRequest(http.MethodPost, "/index.php?action=preview_ecs_create", nil), data)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	summary, ok := result["summary"].(map[string]any)
	if !ok || summary["scheduleEnabled"] != true || summary["startTime"] != "08:00" || summary["stopTime"] != "20:00" || summary["stoppedMode"] != "StopCharging" {
		t.Fatalf("preview settings missing: %#v", result)
	}
	if _, exists := summary["rotationGroupId"]; exists {
		t.Fatalf("preview still exposes rotation group configuration: %#v", summary)
	}
	if _, exists := summary["rotationGroupName"]; exists {
		t.Fatalf("preview still exposes rotation group name: %#v", summary)
	}
	network, _ := summary["network"].(map[string]any)
	securityGroup, _ := network["securityGroup"].(map[string]any)
	if securityGroup["cidr"] != "0.0.0.0/0" || !strings.Contains(fmt.Sprint(securityGroup["rules"]), "所有协议 / 所有端口 / 0.0.0.0/0") {
		t.Fatalf("linux preview does not show all inbound security-group traffic: %#v", securityGroup)
	}
	if !strings.Contains(fmt.Sprint(result["warnings"]), "Linux 安全组将向公网放行所有协议和所有端口") {
		t.Fatalf("linux all-inbound warning missing: %#v", result["warnings"])
	}
}

func TestOnlineUpdateRequestRequiresUpdaterAndPersistsTarget(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "")
	request := httptest.NewRequest(http.MethodPost, "/index.php?action=start_update", nil)
	recorder := httptest.NewRecorder()
	srv.startUpdate(recorder, request, map[string]any{"target_commit": strings.Repeat("a", 40), "target_version": "v1.6.40"})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured updater status: %d", recorder.Code)
	}

	srv.UpdateDir = t.TempDir()
	target := strings.Repeat("b", 40)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/elunez/ecs-controller/releases/tags/v1.6.40":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.6.40", "assets": []map[string]any{{"name": "checksums.txt"}}})
		case "/repos/elunez/ecs-controller/commits/v1.6.40":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": target, "commit": map[string]any{"message": "release build"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	srv.githubAPIBase = api.URL
	srv.packageChecker = func(githubRelease, string) bool { return true }
	recorder = httptest.NewRecorder()
	srv.startUpdate(recorder, request, map[string]any{"target_commit": target, "target_version": "v1.6.40"})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("update request status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(srv.UpdateDir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), target) || !strings.Contains(string(raw), "v1.6.40") {
		t.Fatalf("target commit was not persisted: %s", raw)
	}
}

func TestValidateRotationGroupsRejectsDuplicateNamesAndUnknownInstances(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for index, instanceID := range []string{"i-1", "i-2", "i-3"} {
		if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: instanceID, ScheduleEnabled: true, ScheduleStartEnabled: true, ScheduleStopEnabled: true, StartTime: fmt.Sprintf("%02d:00", 8+index*4), StopTime: fmt.Sprintf("%02d:00", 12+index*4)}); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 3 {
		t.Fatalf("load accounts: %#v %v", accounts, err)
	}
	member := func(index int) app.RotationMember {
		return app.RotationMember{AccountID: accounts[index].ID}
	}
	valid := app.RotationGroup{ID: "g-1", Name: "轮转组", Domain: "a.example.com", Provider: "cloudflare", DNS: app.DNSCredentials{CloudflareToken: "token", TTL: 600}, Members: []app.RotationMember{member(0), member(1)}}
	srv := New(st, t.TempDir(), "")
	empty := app.RotationGroup{ID: "g-empty", Name: "空分组", Domain: "empty.example.com", Provider: "cloudflare", DNS: app.DNSCredentials{CloudflareToken: "token", TTL: 600}}
	single := app.RotationGroup{ID: "g-single", Name: "单机分组", Domain: "single.example.com", Provider: "cloudflare", DNS: app.DNSCredentials{CloudflareToken: "token", TTL: 600}, Members: []app.RotationMember{member(2)}}
	if err := srv.validateRotationGroups([]app.RotationGroup{empty, single}); err != nil {
		t.Fatalf("empty or single-member group rejected: %v", err)
	}
	if err := srv.validateRotationGroups([]app.RotationGroup{valid}); err != nil {
		t.Fatalf("valid group rejected: %v", err)
	}
	duplicateName := valid
	duplicateName.ID = "g-2"
	duplicateName.Domain = "b.example.com"
	duplicateName.Members = []app.RotationMember{member(2), {AccountID: 999}}
	if err := srv.validateRotationGroups([]app.RotationGroup{valid, duplicateName}); err == nil || !strings.Contains(err.Error(), "分组名称不能重复") {
		t.Fatalf("duplicate name was accepted: %v", err)
	}
	valid.Members[1].AccountID = 999
	if err := srv.validateRotationGroups([]app.RotationGroup{valid}); err == nil || !strings.Contains(err.Error(), "分组成员实例无效") {
		t.Fatalf("unknown instance was accepted: %v", err)
	}
}

func TestSaveInstanceScheduleValidatesAndProtectsRotationMembers(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, instanceID := range []string{"i-1", "i-2"} {
		if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: instanceID}); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("load accounts: %#v %v", accounts, err)
	}
	srv := New(st, t.TempDir(), "")
	recorder := httptest.NewRecorder()
	srv.saveInstanceSchedule(recorder, map[string]any{"accountId": accounts[0].ID, "enabled": true, "startTime": "08:00", "stopTime": "18:00", "stoppedMode": "StopCharging"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("save schedule status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	saved, err := st.Account(accounts[0].ID, false)
	if err != nil || !saved.ScheduleEnabled || !saved.ScheduleStartEnabled || !saved.ScheduleStopEnabled || saved.StartTime != "08:00" || saved.StopTime != "18:00" || saved.StoppedMode != "StopCharging" {
		t.Fatalf("schedule was not saved: %#v %v", saved, err)
	}

	recorder = httptest.NewRecorder()
	srv.saveInstanceSchedule(recorder, map[string]any{"accountId": accounts[0].ID, "enabled": true, "startTime": "08:00", "stopTime": "08:00"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("equal times were accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := st.SaveRotationGroups([]app.RotationGroup{{ID: "g-1", Name: "轮转组", Domain: "service.example.com", Provider: "dnspod", DNS: app.DNSCredentials{DNSPodSecretID: "id", DNSPodSecretKey: "key", TTL: 600}, Members: []app.RotationMember{{AccountID: accounts[0].ID}, {AccountID: accounts[1].ID}}}}); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	srv.saveInstanceSchedule(recorder, map[string]any{"accountId": accounts[0].ID, "enabled": false})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "请先从分组移除") {
		t.Fatalf("rotation member schedule was disabled: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSyncGroupPreservesInstanceSchedule(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-1", InstanceStatus: "Stopped", ScheduleEnabled: true, ScheduleStartEnabled: true, ScheduleStopEnabled: true, StartTime: "07:30", StopTime: "19:45", StoppedMode: "StopCharging"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "")
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-1", Status: "Running", PublicIP: "203.0.113.20"}}}
	}
	if count, err := srv.syncGroup("group-1"); err != nil || count != 1 {
		t.Fatalf("sync result: count=%d err=%v", count, err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load accounts: %#v %v", accounts, err)
	}
	account := accounts[0]
	if !account.ScheduleEnabled || !account.ScheduleStartEnabled || !account.ScheduleStopEnabled || account.StartTime != "07:30" || account.StopTime != "19:45" || account.StoppedMode != "StopCharging" {
		t.Fatalf("sync overwrote instance schedule: %#v", account)
	}
}

func TestUpdateStatusCompletesWhenControllerAlreadyRunsTargetCommit(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	updateDir := t.TempDir()
	target := strings.Repeat("a", 40)
	status := fmt.Sprintf(`{"status":"running","phase":"restarting","message":"正在重启 ECS 控制台","progress":94,"target_commit":"%s","current_commit":"%s","request_id":"request-1","updated_at":%d}`, target, target, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(updateDir, "status.json"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}

	previousCommit := app.Commit
	app.Commit = target
	defer func() { app.Commit = previousCommit }()

	srv := New(st, t.TempDir(), "")
	srv.UpdateDir = updateDir
	recorder := httptest.NewRecorder()
	srv.updateStatus(recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "success" || response["phase"] != "completed" || response["progress"] != float64(100) || response["current_commit"] != target {
		t.Fatalf("target commit was not recognized as complete: %#v", response)
	}
}

func postJSON(t *testing.T, client *http.Client, endpoint string, value map[string]any, headers map[string]string) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(value)
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
