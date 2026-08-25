package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type CloudflareClient struct {
	Token      string
	ZoneID     string
	ZoneName   string
	BaseURL    string
	HTTPClient *http.Client
}

func NewCloudflareClient(token, zoneID, zoneName string) *CloudflareClient {
	return &CloudflareClient{Token: strings.TrimSpace(token), ZoneID: strings.TrimSpace(zoneID), ZoneName: normalizeDomain(zoneName), BaseURL: "https://api.cloudflare.com/client/v4", HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

func CloudflareUpdate(ctx context.Context, token, zoneID, name, address string, proxied bool) error {
	return CloudflareUpdateRecord(ctx, token, zoneID, "", name, address, proxied)
}

func CloudflareUpdateRecord(ctx context.Context, token, zoneID, zoneName, name, address string, proxied bool) error {
	client := NewCloudflareClient(token, zoneID, zoneName)
	return client.Update(ctx, name, address, proxied)
}

func CloudflareUpdateRecordWithTTL(ctx context.Context, token, zoneID, zoneName, name, address string, ttl int, proxied bool) error {
	client := NewCloudflareClient(token, zoneID, zoneName)
	return client.UpdateWithTTL(ctx, name, address, ttl, proxied)
}

func CloudflareDelete(ctx context.Context, token, zoneID, name string) error {
	return CloudflareDeleteRecord(ctx, token, zoneID, "", name)
}

func CloudflareDeleteRecord(ctx context.Context, token, zoneID, zoneName, name string) error {
	client := NewCloudflareClient(token, zoneID, zoneName)
	return client.Delete(ctx, name)
}

func (c *CloudflareClient) Update(ctx context.Context, name, address string, proxied bool) error {
	return c.UpdateWithTTL(ctx, name, address, 1, proxied)
}

func (c *CloudflareClient) UpdateWithTTL(ctx context.Context, name, address string, ttl int, proxied bool) error {
	if err := c.validate(name, address); err != nil {
		return err
	}
	zoneID, err := c.resolveZone(ctx)
	if err != nil {
		return err
	}
	records, err := c.records(ctx, zoneID, name)
	if err != nil {
		return err
	}
	if ttl < 60 || proxied {
		ttl = 1
	}
	payload := map[string]any{"type": "A", "name": strings.ToLower(strings.TrimSuffix(name, ".")), "content": address, "ttl": ttl, "proxied": proxied, "comment": "Managed by ECS 控制台"}
	method, endpoint := http.MethodPost, c.endpoint(zoneID, "/dns_records")
	if len(records) > 0 {
		method = http.MethodPut
		endpoint = c.endpoint(zoneID, "/dns_records/"+url.PathEscape(records[0].ID))
	}
	var response apiResponse
	if err := c.requestJSON(ctx, method, endpoint, payload, &response); err != nil {
		return err
	}
	if !response.Success {
		return fmt.Errorf("Cloudflare 更新记录失败: %s", response.errorText())
	}
	return nil
}

func (c *CloudflareClient) Delete(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("Cloudflare 记录名不能为空")
	}
	zoneID, err := c.resolveZone(ctx)
	if err != nil {
		return err
	}
	records, err := c.records(ctx, zoneID, name)
	if err != nil {
		return err
	}
	for _, record := range records {
		var response apiResponse
		if err := c.requestJSON(ctx, http.MethodDelete, c.endpoint(zoneID, "/dns_records/"+url.PathEscape(record.ID)), nil, &response); err != nil {
			return err
		}
		if !response.Success {
			return fmt.Errorf("Cloudflare 删除记录失败: %s", response.errorText())
		}
	}
	return nil
}

type cloudflareRecord struct {
	ID string `json:"id"`
}

type apiResponse struct {
	Success bool             `json:"success"`
	Errors  []map[string]any `json:"errors"`
	Result  json.RawMessage  `json:"result"`
}

func (r apiResponse) errorText() string {
	if len(r.Errors) == 0 {
		return "未知错误"
	}
	items := make([]string, 0, len(r.Errors))
	for _, item := range r.Errors {
		if message, ok := item["message"].(string); ok {
			items = append(items, message)
		}
	}
	if len(items) == 0 {
		return "未知错误"
	}
	return strings.Join(items, "; ")
}

func (c *CloudflareClient) records(ctx context.Context, zoneID, name string) ([]cloudflareRecord, error) {
	var response struct {
		apiResponse
		Result []cloudflareRecord `json:"result"`
	}
	endpoint := c.endpoint(zoneID, "/dns_records?type=A&name="+url.QueryEscape(strings.ToLower(strings.TrimSuffix(name, "."))))
	if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, err
	}
	if !response.Success {
		return nil, fmt.Errorf("Cloudflare 查询记录失败: %s", response.errorText())
	}
	return response.Result, nil
}

func (c *CloudflareClient) resolveZone(ctx context.Context) (string, error) {
	if c.ZoneID != "" {
		return c.ZoneID, nil
	}
	if c.ZoneName == "" {
		return "", fmt.Errorf("Cloudflare Zone ID 或 DDNS 根域名不能为空")
	}
	var response struct {
		apiResponse
		Result []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/zones?name=" + url.QueryEscape(c.ZoneName) + "&status=active&per_page=1"
	if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return "", err
	}
	if !response.Success || len(response.Result) == 0 || response.Result[0].ID == "" {
		return "", fmt.Errorf("Cloudflare 未找到域名 %s 对应的 Zone", c.ZoneName)
	}
	c.ZoneID = response.Result[0].ID
	return c.ZoneID, nil
}

func (c *CloudflareClient) requestJSON(ctx context.Context, method, endpoint string, payload any, output any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Cloudflare HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return err
	}
	return nil
}

func (c *CloudflareClient) endpoint(zoneID, path string) string {
	return strings.TrimRight(c.BaseURL, "/") + "/zones/" + url.PathEscape(zoneID) + path
}

func (c *CloudflareClient) validate(name, address string) error {
	if c.Token == "" {
		return fmt.Errorf("Cloudflare token 不能为空")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("Cloudflare 记录名不能为空")
	}
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return fmt.Errorf("公网 IP 为空或不是公网 IPv4")
	}
	return nil
}

func normalizeDomain(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	if index := strings.IndexByte(value, '/'); index >= 0 {
		value = value[:index]
	}
	return strings.Trim(value, ".")
}
