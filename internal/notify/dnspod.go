package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type DNSPodClient struct {
	SecretID   string
	SecretKey  string
	Endpoint   string
	HTTPClient *http.Client
}

func NewDNSPodClient(secretID, secretKey string) *DNSPodClient {
	return &DNSPodClient{SecretID: strings.TrimSpace(secretID), SecretKey: strings.TrimSpace(secretKey), Endpoint: "https://dnspod.tencentcloudapi.com", HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

func (c *DNSPodClient) Update(ctx context.Context, domain, subdomain, address string, ttl int) error {
	if c.SecretID == "" || c.SecretKey == "" {
		return fmt.Errorf("DNSPod SecretId 或 SecretKey 不能为空")
	}
	if ttl < 60 {
		ttl = 600
	}
	var listed struct {
		Response struct {
			RecordList []struct {
				RecordID uint64 `json:"RecordId"`
				Line     string `json:"Line"`
			} `json:"RecordList"`
			Error *dnsPodError `json:"Error"`
		} `json:"Response"`
	}
	if err := c.call(ctx, "DescribeRecordList", map[string]any{"Domain": domain, "Subdomain": subdomain, "RecordType": "A", "Limit": 100}, &listed); err != nil {
		return err
	}
	if listed.Response.Error != nil {
		return listed.Response.Error
	}
	payload := map[string]any{"Domain": domain, "SubDomain": subdomain, "RecordType": "A", "RecordLine": "默认", "Value": address, "TTL": ttl}
	action := "CreateRecord"
	if len(listed.Response.RecordList) > 0 {
		action = "ModifyRecord"
		payload["RecordId"] = listed.Response.RecordList[0].RecordID
		if listed.Response.RecordList[0].Line != "" {
			payload["RecordLine"] = listed.Response.RecordList[0].Line
		}
	}
	var result struct {
		Response struct {
			Error *dnsPodError `json:"Error"`
		} `json:"Response"`
	}
	if err := c.call(ctx, action, payload, &result); err != nil {
		return err
	}
	if result.Response.Error != nil {
		return result.Response.Error
	}
	return nil
}

type dnsPodError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

func (e *dnsPodError) Error() string { return fmt.Sprintf("DNSPod %s: %s", e.Code, e.Message) }

func (c *DNSPodClient) call(ctx context.Context, action string, payload any, output any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	timestamp := time.Now().Unix()
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")
	host := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(c.Endpoint, "/"), "https://"), "http://")
	canonicalHeaders := "content-type:application/json; charset=utf-8\nhost:" + host + "\n"
	hashedPayload := sha256Hex(raw)
	canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\ncontent-type;host\n" + hashedPayload
	credentialScope := date + "/dnspod/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" + fmt.Sprint(timestamp) + "\n" + credentialScope + "\n" + sha256Hex([]byte(canonicalRequest))
	secretDate := hmacSHA256([]byte("TC3"+c.SecretKey), date)
	secretService := hmacSHA256(secretDate, "dnspod")
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))
	authorization := "TC3-HMAC-SHA256 Credential=" + c.SecretID + "/" + credentialScope + ", SignedHeaders=content-type;host, Signature=" + signature
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", authorization)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", "2021-03-23")
	req.Header.Set("X-TC-Timestamp", fmt.Sprint(timestamp))
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("DNSPod HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, output)
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(value))
	return h.Sum(nil)
}
