package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// TelegramClient is the small subset of the Bot API used by notifications and
// the optional control worker. Keeping the transport injectable makes polling
// testable without contacting Telegram.
type TelegramClient struct {
	Token      string
	BaseURL    string
	HTTPClient *http.Client
}

type TelegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func NewTelegramClient(token, proxyType, customURL, proxyHost, proxyPort, proxyUser, proxyPass string) (*TelegramClient, error) {
	token = strings.TrimSpace(token)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("Telegram token 不能为空")
	}
	// 不设全局 Timeout，由每次调用的 context 控制超时，避免 getUpdates 长轮询被截断
	client := &http.Client{}
	if proxyType == "custom" && strings.TrimSpace(customURL) != "" {
		base := strings.TrimRight(customURL, "/")
		return &TelegramClient{Token: token, BaseURL: base, HTTPClient: client}, nil
	}
	if proxyType == "socks5" && proxyHost != "" && proxyPort != "" {
		dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(proxyHost, proxyPort), auth(proxyUser, proxyPass), proxy.Direct)
		if err != nil {
			return nil, err
		}
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.Dial(network, address)
		}}
	}
	return &TelegramClient{Token: token, BaseURL: "https://api.telegram.org", HTTPClient: client}, nil
}

func auth(user, password string) *proxy.Auth {
	if user == "" && password == "" {
		return nil
	}
	return &proxy.Auth{User: user, Password: password}
}

func (c *TelegramClient) Call(ctx context.Context, method string, values url.Values) (TelegramResponse, error) {
	if c == nil || c.Token == "" {
		return TelegramResponse{}, fmt.Errorf("Telegram token 不能为空")
	}
	// 如果调用方没有设置 deadline，给一个 30 秒的默认超时兜底
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/bot"+c.Token+"/"+method, strings.NewReader(values.Encode()))
	if err != nil {
		return TelegramResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return TelegramResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return TelegramResponse{}, err
	}
	var result TelegramResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return TelegramResponse{}, fmt.Errorf("Telegram 返回异常 HTTP %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 || !result.OK {
		if resp.StatusCode == http.StatusNotFound && strings.EqualFold(strings.TrimSpace(result.Description), "Not Found") {
			return result, fmt.Errorf("Telegram Bot Token 无效或机器人不存在（API 返回 404 Not Found）")
		}
		return result, fmt.Errorf("Telegram: %s", result.Description)
	}
	return result, nil
}

func (c *TelegramClient) SendMessage(ctx context.Context, chatID, message string, keyboard any) error {
	values := url.Values{"chat_id": {chatID}, "text": {message}}
	if keyboard != nil {
		raw, err := json.Marshal(keyboard)
		if err != nil {
			return err
		}
		values.Set("reply_markup", string(raw))
	}
	_, err := c.Call(ctx, "sendMessage", values)
	return err
}

func (c *TelegramClient) EditMessage(ctx context.Context, chatID, messageID, message string, keyboard any) error {
	values := url.Values{"chat_id": {chatID}, "message_id": {messageID}, "text": {message}}
	if keyboard != nil {
		raw, err := json.Marshal(keyboard)
		if err != nil {
			return err
		}
		values.Set("reply_markup", string(raw))
	}
	_, err := c.Call(ctx, "editMessageText", values)
	return err
}

func (c *TelegramClient) AnswerCallback(ctx context.Context, id, message string) error {
	values := url.Values{"callback_query_id": {id}}
	if message != "" {
		values.Set("text", message)
	}
	_, err := c.Call(ctx, "answerCallbackQuery", values)
	return err
}

func (c *TelegramClient) GetUpdates(ctx context.Context, offset int64) ([]map[string]any, error) {
	const pollTimeout = 20 // Telegram 服务端长轮询等待秒数
	values := url.Values{"offset": {strconv.FormatInt(offset, 10)}, "limit": {"20"}, "timeout": {strconv.Itoa(pollTimeout)}, "allowed_updates": {`["message","callback_query"]`}}
	// 客户端超时 = 服务端长轮询时间 + 15 秒网络缓冲，防止代理或慢网络导致 Client.Timeout 截断
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(pollTimeout+15)*time.Second)
	defer cancel()
	response, err := c.Call(pollCtx, "getUpdates", values)
	if err != nil {
		return nil, err
	}
	var updates []map[string]any
	if len(response.Result) == 0 {
		return updates, nil
	}
	if err := json.Unmarshal(response.Result, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func Webhook(ctx context.Context, endpoint, method, contentType string, headers map[string]string, body any) error {
	if endpoint == "" {
		return fmt.Errorf("webhook URL 不能为空")
	}
	var data []byte
	var err error
	if strings.EqualFold(contentType, "form") {
		values := url.Values{}
		if fields, ok := body.(map[string]string); ok {
			for k, v := range fields {
				values.Set(k, v)
			}
		}
		data = []byte(values.Encode())
	} else {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, methodOrDefault(method), endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if contentType == "" || strings.EqualFold(contentType, "json") {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

func Telegram(ctx context.Context, token, chatID, message string) error {
	if chatID == "" {
		return fmt.Errorf("Telegram token/chat_id 不能为空")
	}
	client, err := NewTelegramClient(token, "", "", "", "", "", "")
	if err != nil {
		return err
	}
	return client.SendMessage(ctx, chatID, message, nil)
}

func Email(ctx context.Context, host string, port int, username, password, from, to, subject, body, secure string) error {
	if host == "" || to == "" {
		return fmt.Errorf("SMTP host/收件人不能为空")
	}
	if port == 0 {
		port = 465
	}
	if from == "" {
		from = username
	}
	addr := host + ":" + strconv.Itoa(port)
	msg := []byte("To: " + to + "\r\nSubject: " + subject + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + body + "\r\n")
	auth := smtp.PlainAuth("", username, password, host)
	if secure == "ssl" {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		defer client.Close()
		if err = client.Auth(auth); err != nil {
			return err
		}
		if err = client.Mail(from); err != nil {
			return err
		}
		if err = client.Rcpt(to); err != nil {
			return err
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		if _, err = w.Write(msg); err != nil {
			return err
		}
		return w.Close()
	}
	return smtp.SendMail(addr, auth, from, []string{to}, msg)
}

func methodOrDefault(method string) string {
	if method == "" {
		return http.MethodPost
	}
	return strings.ToUpper(method)
}
