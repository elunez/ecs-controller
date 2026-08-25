package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
)

const defaultUpdateRepo = "elunez/ecs-controller"

var commitPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var versionTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

type githubCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
	HTMLURL string `json:"html_url"`
}

type updateRequest struct {
	RequestID     string `json:"request_id"`
	TargetSHA     string `json:"target_sha"`
	TargetVersion string `json:"target_version"`
	RequestedAt   int64  `json:"requested_at"`
}

func (s *Server) updateRepository() string {
	return fallback(os.Getenv("ECS_UPDATE_REPO"), defaultUpdateRepo)
}

func (s *Server) updateRepositoryURL() string {
	return "https://github.com/" + s.updateRepository()
}

func (s *Server) updateAPIURL() string {
	if base := strings.TrimRight(strings.TrimSpace(s.githubAPIBase), "/"); base != "" {
		return base
	}
	return "https://api.github.com"
}

func (s *Server) updateConfigured() bool {
	return strings.TrimSpace(s.UpdateDir) != ""
}

func releaseAssetName(goos, goarch string) string {
	if goarch != "amd64" && goarch != "arm64" {
		return ""
	}
	switch goos {
	case "linux":
		return "ecs-controller-linux-" + goarch + ".tar.gz"
	case "windows":
		return "ecs-controller-windows-" + goarch + ".zip"
	default:
		return ""
	}
}

func releaseHasAssets(release githubRelease, assetName string) bool {
	if assetName == "" {
		return false
	}
	foundPackage, foundChecksums := false, false
	for _, asset := range release.Assets {
		switch asset.Name {
		case assetName:
			foundPackage = true
		case "checksums.txt":
			foundChecksums = true
		}
	}
	return foundPackage && foundChecksums
}

func (s *Server) releasePackageAvailable(release githubRelease, assetName string) bool {
	if s.packageChecker != nil {
		return s.packageChecker(release, assetName)
	}
	return releaseHasAssets(release, assetName)
}

func (s *Server) githubJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "ecs-controller-update-check")
	response, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub 返回 HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(target); err != nil {
		return fmt.Errorf("GitHub 返回的数据无效: %w", err)
	}
	return nil
}

func (s *Server) latestRelease(ctx context.Context) (githubRelease, githubCommit, error) {
	var release githubRelease
	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", s.updateAPIURL(), s.updateRepository())
	if err := s.githubJSON(ctx, endpoint, &release); err != nil {
		return githubRelease{}, githubCommit{}, err
	}
	return s.resolveReleaseCommit(ctx, release)
}

func (s *Server) releaseForVersion(ctx context.Context, version string) (githubRelease, githubCommit, error) {
	var release githubRelease
	endpoint := fmt.Sprintf("%s/repos/%s/releases/tags/%s", s.updateAPIURL(), s.updateRepository(), url.PathEscape(version))
	if err := s.githubJSON(ctx, endpoint, &release); err != nil {
		return githubRelease{}, githubCommit{}, err
	}
	if release.TagName != version {
		return githubRelease{}, githubCommit{}, errors.New("GitHub Release 版本不匹配")
	}
	return s.resolveReleaseCommit(ctx, release)
}

func (s *Server) resolveReleaseCommit(ctx context.Context, release githubRelease) (githubRelease, githubCommit, error) {
	if !versionTagPattern.MatchString(release.TagName) {
		return githubRelease{}, githubCommit{}, errors.New("GitHub Release 版本无效")
	}
	var commit githubCommit
	endpoint := fmt.Sprintf("%s/repos/%s/commits/%s", s.updateAPIURL(), s.updateRepository(), url.PathEscape(release.TagName))
	if err := s.githubJSON(ctx, endpoint, &commit); err != nil {
		return githubRelease{}, githubCommit{}, err
	}
	if !commitPattern.MatchString(commit.SHA) {
		return githubRelease{}, githubCommit{}, errors.New("GitHub Release 提交版本无效")
	}
	return release, commit, nil
}

func (s *Server) checkForUpdate(w http.ResponseWriter, r *http.Request) {
	currentCommit := strings.TrimSpace(app.Commit)
	currentVersion := strings.TrimSpace(app.Version)
	if currentVersion == "" || currentVersion == "dev" {
		currentVersion = fallback(shortCommit(currentCommit), "dev")
	}
	assetName := releaseAssetName(runtime.GOOS, runtime.GOARCH)
	result := map[string]any{
		"success":                 true,
		"configured":              s.updateConfigured(),
		"repository":              s.updateRepository(),
		"repository_url":          s.updateRepositoryURL(),
		"current_version":         currentVersion,
		"current_commit":          currentCommit,
		"current_url":             "",
		"build_date":              app.BuildDate,
		"platform":                runtime.GOOS + "/" + runtime.GOARCH,
		"package_name":            assetName,
		"package_available":       false,
		"source_update_available": false,
		"update_available":        false,
		"checked_at":              time.Now().UTC().Format(time.RFC3339),
	}
	if commitPattern.MatchString(currentCommit) {
		result["current_url"] = s.updateRepositoryURL() + "/commit/" + currentCommit
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	release, latest, err := s.latestRelease(ctx)
	if err != nil {
		result["success"] = false
		result["message"] = "无法获取 GitHub Release：" + err.Error()
		s.json(w, http.StatusOK, result)
		return
	}
	packageAvailable := s.releasePackageAvailable(release, assetName)
	updateAvailable := !strings.EqualFold(currentCommit, latest.SHA)
	result["latest"] = map[string]any{
		"version": release.TagName,
		"commit":  latest.SHA,
		"message": strings.TrimSpace(strings.Split(latest.Commit.Message, "\n")[0]),
		"url":     fallback(release.HTMLURL, latest.HTMLURL),
	}
	result["package_available"] = packageAvailable
	result["source_update_available"] = updateAvailable
	result["update_available"] = updateAvailable && packageAvailable
	if updateAvailable && !packageAvailable {
		if assetName == "" {
			result["message"] = "当前平台不支持后台在线更新"
		} else {
			result["message"] = "检测到新版本，但对应的发布包或校验文件尚未就绪"
		}
	}
	if !s.updateConfigured() {
		result["message"] = "当前部署未启用后台在线更新，请重新运行 install.sh"
	}
	s.json(w, http.StatusOK, result)
}

func (s *Server) updateStatus(w http.ResponseWriter) {
	currentCommit := strings.TrimSpace(app.Commit)
	status := map[string]any{
		"status":         "idle",
		"configured":     s.updateConfigured(),
		"current_commit": currentCommit,
	}
	if !s.updateConfigured() {
		status["message"] = "当前部署未启用后台在线更新"
		s.json(w, http.StatusOK, status)
		return
	}
	path := filepath.Join(s.UpdateDir, "status.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		var stored map[string]any
		if json.Unmarshal(raw, &stored) == nil {
			for key, value := range stored {
				status[key] = value
			}
			targetCommit := strings.TrimSpace(stringValue(stored["target_commit"]))
			storedStatus := strings.TrimSpace(stringValue(stored["status"]))
			if commitPattern.MatchString(currentCommit) && strings.EqualFold(targetCommit, currentCommit) && (storedStatus == "queued" || storedStatus == "running") {
				status["status"] = "success"
				status["phase"] = "completed"
				status["message"] = "更新完成，当前已运行最新版本"
				status["progress"] = 100
				status["current_commit"] = currentCommit
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		status["message"] = "更新状态读取失败"
	}
	s.json(w, http.StatusOK, status)
}

func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request, data map[string]any) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if !s.updateConfigured() {
		s.error(w, http.StatusServiceUnavailable, "当前部署未启用后台在线更新，请重新运行 install.sh")
		return
	}
	targetSHA := strings.ToLower(strings.TrimSpace(stringValue(data["target_commit"])))
	targetVersion := strings.TrimSpace(stringValue(data["target_version"]))
	if !commitPattern.MatchString(targetSHA) || !versionTagPattern.MatchString(targetVersion) {
		s.error(w, http.StatusBadRequest, "更新版本标识无效，请重新检查更新")
		return
	}
	currentCommit := strings.TrimSpace(app.Commit)
	if commitPattern.MatchString(currentCommit) && strings.EqualFold(currentCommit, targetSHA) {
		s.error(w, http.StatusConflict, "当前已经是目标版本")
		return
	}

	checkContext, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	release, commit, err := s.releaseForVersion(checkContext, targetVersion)
	if err != nil {
		s.error(w, http.StatusServiceUnavailable, "无法确认目标 GitHub Release，请稍后重试")
		return
	}
	if !strings.EqualFold(commit.SHA, targetSHA) {
		s.error(w, http.StatusConflict, "GitHub Release 与目标提交不一致，请重新检查更新")
		return
	}
	if !s.releasePackageAvailable(release, releaseAssetName(runtime.GOOS, runtime.GOARCH)) {
		s.error(w, http.StatusConflict, "目标版本的发布包或校验文件尚未就绪")
		return
	}

	state := s.readUpdateState()
	if state == "queued" || state == "running" {
		s.error(w, http.StatusConflict, "已有更新任务正在执行")
		return
	}
	if _, err := os.Stat(filepath.Join(s.UpdateDir, "request.json")); err == nil {
		s.error(w, http.StatusConflict, "已有更新请求等待执行")
		return
	}
	if _, err := os.Stat(filepath.Join(s.UpdateDir, "request.processing.json")); err == nil {
		s.error(w, http.StatusConflict, "已有更新请求等待执行")
		return
	}
	if err := os.MkdirAll(s.UpdateDir, 0700); err != nil {
		s.error(w, http.StatusInternalServerError, "更新目录不可用")
		return
	}
	request := updateRequest{
		RequestID:     randomToken(16),
		TargetSHA:     targetSHA,
		TargetVersion: targetVersion,
		RequestedAt:   time.Now().Unix(),
	}
	path := filepath.Join(s.UpdateDir, "request.json")
	temporary := path + ".tmp"
	raw, _ := json.Marshal(request)
	if err := os.WriteFile(temporary, raw, 0600); err != nil {
		s.error(w, http.StatusInternalServerError, "更新请求写入失败")
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		s.error(w, http.StatusInternalServerError, "更新请求提交失败")
		return
	}
	s.json(w, http.StatusAccepted, map[string]any{
		"success":        true,
		"request_id":     request.RequestID,
		"status":         "queued",
		"target_version": targetVersion,
	})
}

func (s *Server) readUpdateState() string {
	if !s.updateConfigured() {
		return "idle"
	}
	raw, err := os.ReadFile(filepath.Join(s.UpdateDir, "status.json"))
	if err != nil {
		return "idle"
	}
	var state struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return "idle"
	}
	return state.Status
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}
