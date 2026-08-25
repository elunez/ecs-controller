package notify

import (
	"context"
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"
)

type DNSUpdateConfig struct {
	Provider              string
	Domain                string
	TTL                   int
	CloudflareToken       string
	CloudflareProxied     bool
	DNSPodSecretID        string
	DNSPodSecretKey       string
	AliDNSAccessKeyID     string
	AliDNSAccessKeySecret string
}

func UpdateDNSRecord(ctx context.Context, config DNSUpdateConfig, address string) error {
	domain := strings.ToLower(strings.Trim(strings.TrimSpace(config.Domain), "."))
	if net.ParseIP(strings.TrimSpace(address)) == nil {
		return fmt.Errorf("公网 IP 无效")
	}
	root, record, err := splitDNSName(domain)
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(config.Provider)) {
	case "cloudflare":
		return CloudflareUpdateRecordWithTTL(ctx, config.CloudflareToken, "", root, domain, address, config.TTL, config.CloudflareProxied)
	case "dnspod":
		return NewDNSPodClient(config.DNSPodSecretID, config.DNSPodSecretKey).Update(ctx, root, record, address, config.TTL)
	case "alidns":
		return NewAliDNSClient(config.AliDNSAccessKeyID, config.AliDNSAccessKeySecret).Update(ctx, root, record, address, config.TTL)
	default:
		return fmt.Errorf("不支持的 DNS 服务商")
	}
}

func splitDNSName(domain string) (string, string, error) {
	root, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return "", "", fmt.Errorf("共享域名无效: %w", err)
	}
	record := strings.TrimSuffix(domain, "."+root)
	if record == domain || record == "" {
		record = "@"
	}
	return root, record, nil
}
