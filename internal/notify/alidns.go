package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/cloud"
)

type AliDNSClient struct {
	RPC *cloud.RPCClient
}

func NewAliDNSClient(accessKeyID, accessKeySecret string) *AliDNSClient {
	return &AliDNSClient{RPC: &cloud.RPCClient{HTTPClient: &http.Client{Timeout: 15 * time.Second}, Endpoint: "https://alidns.aliyuncs.com/", Version: "2015-01-09", Product: "Alidns", AccessKey: strings.TrimSpace(accessKeyID), Secret: strings.TrimSpace(accessKeySecret)}}
}

func (c *AliDNSClient) Update(ctx context.Context, domain, record, address string, ttl int) error {
	if c == nil || c.RPC == nil || c.RPC.AccessKey == "" || c.RPC.Secret == "" {
		return fmt.Errorf("阿里云 DNS AccessKey ID 或 Secret 不能为空")
	}
	if ttl < 60 {
		ttl = 600
	}
	result, err := c.RPC.Call(ctx, "DescribeDomainRecords", map[string]string{"DomainName": domain, "RRKeyWord": record, "Type": "A", "PageSize": "100"})
	if err != nil {
		return err
	}
	recordID := aliDNSRecordID(result, record)
	params := map[string]string{"RR": record, "Type": "A", "Value": address, "TTL": fmt.Sprint(ttl)}
	action := "AddDomainRecord"
	if recordID != "" {
		action = "UpdateDomainRecord"
		params["RecordId"] = recordID
	} else {
		params["DomainName"] = domain
	}
	_, err = c.RPC.Call(ctx, action, params)
	return err
}

func aliDNSRecordID(result map[string]any, record string) string {
	domainRecords, _ := result["DomainRecords"].(map[string]any)
	items, _ := domainRecords["Record"].([]any)
	for _, item := range items {
		entry, _ := item.(map[string]any)
		if strings.EqualFold(fmt.Sprint(entry["RR"]), record) && strings.EqualFold(fmt.Sprint(entry["Type"]), "A") {
			return fmt.Sprint(entry["RecordId"])
		}
	}
	return ""
}
