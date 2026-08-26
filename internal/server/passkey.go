package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	webauthn "github.com/go-webauthn/webauthn/webauthn"
)

const (
	passkeyChallengeCookie = "ecs_passkey_challenge"
	passkeyRegisterKind    = "register"
	passkeyLoginKind       = "login"
	passkeyUserHandle      = "ecs-controller-admin"
)

type adminPasskeyUser struct {
	username    string
	credentials webauthn.Credentials
}

func (u *adminPasskeyUser) WebAuthnID() []byte { return []byte(passkeyUserHandle) }

func (u *adminPasskeyUser) WebAuthnName() string { return u.username }

func (u *adminPasskeyUser) WebAuthnDisplayName() string { return "ECS 控制台管理员" }

func (u *adminPasskeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func (s *Server) passkeyUser() (*adminPasskeyUser, error) {
	username := strings.TrimSpace(s.Store.GetSetting("admin_username", ""))
	if username == "" {
		return nil, fmt.Errorf("passkey user has no admin username")
	}
	records, err := s.Store.PasskeyCredentials()
	if err != nil {
		return nil, err
	}
	user := &adminPasskeyUser{username: username, credentials: make(webauthn.Credentials, 0, len(records))}
	for _, record := range records {
		var credential webauthn.Credential
		if err := json.Unmarshal([]byte(record.Data), &credential); err != nil {
			return nil, fmt.Errorf("decode passkey credential: %w", err)
		}
		if len(credential.ID) == 0 || record.CredentialID == "" {
			return nil, fmt.Errorf("passkey credential has no id")
		}
		user.credentials = append(user.credentials, credential)
	}
	return user, nil
}

func (s *Server) passkeyWebAuthn(r *http.Request) (*webauthn.WebAuthn, error) {
	host := forwardedHost(r)
	if host == "" {
		return nil, fmt.Errorf("WebAuthn 请求缺少主机名")
	}

	// Use the browser-facing host and protocol forwarded by the reverse proxy.
	// This keeps normal deployments configuration-free while preserving the
	// exact origin required by WebAuthn's client-data validation.
	rpID := hostWithoutPort(host)
	origins := []string{requestOrigin(r, host)}

	return webauthn.New(&webauthn.Config{
		RPDisplayName: "ECS 控制台",
		RPID:          rpID,
		RPOrigins:     origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
		},
	})
}

func (s *Server) passkeyStatus(w http.ResponseWriter) {
	count := s.Store.PasskeyCount()
	s.json(w, http.StatusOK, map[string]any{
		"enabled": count > 0,
		"count":   count,
	})
}

func (s *Server) passkeyRegisterStart(w http.ResponseWriter, r *http.Request) {
	sessionID := cookieID(r)
	if sessionID == "" {
		s.error(w, http.StatusForbidden, "登录会话已失效，请重新登录")
		return
	}
	user, err := s.passkeyUser()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 凭据读取失败")
		return
	}
	wa, err := s.passkeyWebAuthn(r)
	if err != nil {
		s.error(w, http.StatusBadRequest, passkeyConfigError(err))
		return
	}
	creation, session, err := wa.BeginRegistration(user,
		webauthn.WithExclusions(webauthn.Credentials(user.WebAuthnCredentials()).CredentialDescriptors()),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			RequireResidentKey: protocol.ResidentKeyRequired(),
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationRequired,
		}),
		webauthn.WithExtensions(map[string]any{"credProps": true}),
	)
	if err != nil {
		s.error(w, http.StatusBadRequest, passkeyConfigError(err))
		return
	}
	if err := s.savePasskeyChallenge(w, passkeyRegisterKind, sessionID, session); err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 挑战保存失败")
		return
	}
	s.json(w, http.StatusOK, creation)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	challengeID := passkeyChallengeID(r)
	s.clearPasskeyChallenge(w)
	sessionID := cookieID(r)
	serialized, ok, err := s.Store.ConsumePasskeyChallenge(challengeID, passkeyRegisterKind, sessionID)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 挑战读取失败")
		return
	}
	if !ok {
		s.error(w, http.StatusBadRequest, "Passkey 挑战已过期，请重新开始注册")
		return
	}
	var session webauthn.SessionData
	if err := json.Unmarshal([]byte(serialized), &session); err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 挑战格式无效")
		return
	}
	user, err := s.passkeyUser()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 凭据读取失败")
		return
	}
	wa, err := s.passkeyWebAuthn(r)
	if err != nil {
		s.error(w, http.StatusBadRequest, passkeyConfigError(err))
		return
	}
	credential, err := wa.FinishRegistration(user, session, r)
	if err != nil {
		s.Log.Printf("passkey registration failed: %v", err)
		s.error(w, http.StatusBadRequest, "Passkey 注册失败，请确认使用的是当前网站的安全验证")
		return
	}
	data, err := json.Marshal(credential)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 凭据编码失败")
		return
	}
	if err := s.Store.SavePasskeyCredential(credentialID(credential.ID), string(data)); err != nil {
		s.error(w, http.StatusBadRequest, "这个 Passkey 已经注册过了，或保存失败")
		return
	}
	s.json(w, http.StatusOK, map[string]any{"success": true, "count": s.Store.PasskeyCount()})
}

func (s *Server) passkeyLoginStart(w http.ResponseWriter, r *http.Request) {
	if s.Store.PasskeyCount() == 0 {
		s.error(w, http.StatusBadRequest, "尚未设置 Passkey，请先使用密码登录并完成注册")
		return
	}
	wa, err := s.passkeyWebAuthn(r)
	if err != nil {
		s.error(w, http.StatusBadRequest, passkeyConfigError(err))
		return
	}
	assertion, session, err := wa.BeginDiscoverableMediatedLogin(protocol.MediationDefault,
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		s.error(w, http.StatusBadRequest, passkeyConfigError(err))
		return
	}
	if err := s.savePasskeyChallenge(w, passkeyLoginKind, "", session); err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 挑战保存失败")
		return
	}
	s.json(w, http.StatusOK, assertion)
}

func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if s.Store.RecentLoginFailures(ip, time.Minute) >= 10 {
		s.error(w, http.StatusTooManyRequests, "登录尝试过于频繁")
		return
	}
	challengeID := passkeyChallengeID(r)
	s.clearPasskeyChallenge(w)
	serialized, ok, err := s.Store.ConsumePasskeyChallenge(challengeID, passkeyLoginKind, "")
	if err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 挑战读取失败")
		return
	}
	if !ok {
		s.error(w, http.StatusBadRequest, "Passkey 挑战已过期，请重新开始登录")
		return
	}
	var session webauthn.SessionData
	if err := json.Unmarshal([]byte(serialized), &session); err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 挑战格式无效")
		return
	}
	user, err := s.passkeyUser()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 凭据读取失败")
		return
	}
	wa, err := s.passkeyWebAuthn(r)
	if err != nil {
		s.error(w, http.StatusBadRequest, passkeyConfigError(err))
		return
	}
	validatedUser, credential, err := wa.FinishPasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		if !bytes.Equal(user.WebAuthnID(), userHandle) {
			return nil, fmt.Errorf("unknown passkey user")
		}
		return user, nil
	}, session, r)
	if err != nil {
		s.Store.RecordLoginFailure(ip)
		s.Log.Printf("passkey login failed: %v", err)
		s.error(w, http.StatusUnauthorized, "Passkey 验证失败，请重试")
		return
	}
	adminUser, ok := validatedUser.(*adminPasskeyUser)
	if !ok || credential == nil || len(adminUser.credentials) == 0 {
		s.Store.RecordLoginFailure(ip)
		s.error(w, http.StatusUnauthorized, "Passkey 验证失败，请重试")
		return
	}
	data, err := json.Marshal(credential)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 凭据编码失败")
		return
	}
	if err := s.Store.UpdatePasskeyCredential(credentialID(credential.ID), string(data)); err != nil {
		s.error(w, http.StatusInternalServerError, "Passkey 使用记录保存失败")
		return
	}
	s.Store.ClearLoginFailures(ip)
	s.createSession(w)
	s.json(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) savePasskeyChallenge(w http.ResponseWriter, kind, sessionID string, session *webauthn.SessionData) error {
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	id := randomToken(32)
	if err := s.Store.SavePasskeyChallenge(id, kind, sessionID, string(data), 5*time.Minute); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     passkeyChallengeCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300,
	})
	return nil
}

func (s *Server) clearPasskeyChallenge(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: passkeyChallengeCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.CookieSecure, SameSite: http.SameSiteLaxMode})
}

func passkeyChallengeID(r *http.Request) string {
	cookie, err := r.Cookie(passkeyChallengeCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func credentialID(id []byte) string {
	return base64.RawURLEncoding.EncodeToString(id)
}

func passkeyConfigError(err error) string {
	if strings.Contains(err.Error(), "RPID") || strings.Contains(err.Error(), "origin") || strings.Contains(err.Error(), "domain") {
		return "Passkey 配置无效，请确认当前域名使用 HTTPS，并检查反向代理的 Host、X-Forwarded-Host 和 X-Forwarded-Proto"
	}
	return "Passkey 服务暂不可用，请稍后重试"
}

func forwardedHost(r *http.Request) string {
	host := firstForwardedValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return strings.TrimSpace(host)
}

func requestOrigin(r *http.Request, host string) string {
	scheme := firstForwardedValue(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + host
}

func firstForwardedValue(value string) string {
	if index := strings.IndexByte(value, ','); index >= 0 {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}

func hostWithoutPort(host string) string {
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(parsed, "[]")
	}
	return strings.Trim(host, "[]")
}
