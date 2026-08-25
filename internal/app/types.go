package app

import "time"

func ResolveShutdownMode(value, fallback string) string {
	if value == "KeepCharging" || value == "StopCharging" {
		return value
	}
	if fallback == "StopCharging" {
		return fallback
	}
	return "KeepCharging"
}

type AccountGroup struct {
	GroupKey                 string  `json:"groupKey"`
	AccessKeyID              string  `json:"AccessKeyId"`
	AccessKeySecret          string  `json:"AccessKeySecret"`
	RegionID                 string  `json:"regionId"`
	SiteType                 string  `json:"siteType"`
	MaxTraffic               float64 `json:"maxTraffic"`
	Remark                   string  `json:"remark"`
	ScheduleBlockedByTraffic bool    `json:"scheduleBlockedByTraffic"`
}

type RotationMember struct {
	AccountID int64 `json:"accountId"`
}

type DNSCredentials struct {
	CloudflareToken   string `json:"cloudflareToken"`
	CloudflareProxied bool   `json:"cloudflareProxied"`
	DNSPodSecretID    string `json:"dnspodSecretId"`
	DNSPodSecretKey   string `json:"dnspodSecretKey"`
	AliDNSAccessKeyID string `json:"alidnsAccessKeyId"`
	AliDNSSecret      string `json:"alidnsAccessKeySecret"`
	TTL               int    `json:"ttl"`
}

type RotationGroup struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	Domain              string           `json:"domain"`
	Provider            string           `json:"provider"`
	AdvanceStartMinutes int              `json:"advanceStartMinutes"`
	DelayStopMinutes    int              `json:"delayStopMinutes"`
	DNS                 DNSCredentials   `json:"dns"`
	Members             []RotationMember `json:"members"`
}

type RotationState struct {
	GroupID        string
	AccountID      int64
	LastStartDate  string
	LastStopDate   string
	DNSUpdatedDate string
	LastError      string
}

type Account struct {
	ID                       int64
	AccessKeyID              string
	AccessKeySecret          string
	RegionID                 string
	InstanceID               string
	MaxTraffic               float64
	ScheduleEnabled          bool
	ScheduleStartEnabled     bool
	ScheduleStopEnabled      bool
	StartTime                string
	StopTime                 string
	TrafficUsed              float64
	TrafficBillingMonth      string
	InstanceStatus           string
	UpdatedAt                int64
	LastKeepAliveAt          int64
	AutoStartBlocked         bool
	ScheduleLastStartDate    string
	ScheduleLastStopDate     string
	ScheduleStopActive       bool
	ScheduleBlockedByTraffic bool
	Remark                   string
	SiteType                 string
	GroupKey                 string
	InstanceName             string
	InstanceType             string
	InternetBandwidth        int
	PublicIP                 string
	PublicIPMode             string
	EIPAllocationID          string
	EIPAddress               string
	EIPManaged               bool
	PrivateIP                string
	CPU                      int
	Memory                   int
	OSName                   string
	StoppedMode              string
	HealthStatus             string
	TrafficAPIStatus         string
	TrafficAPIMessage        string
	ProtectionSuspended      bool
	ProtectionSuspendReason  string
	ProtectionNotifiedAt     int64
	IsDeleted                int
}

type EcsTask struct {
	TaskID        string         `json:"task_id"`
	PreviewID     string         `json:"preview_id"`
	GroupKey      string         `json:"account_group_key"`
	RegionID      string         `json:"region_id"`
	InstanceType  string         `json:"instance_type"`
	Status        string         `json:"status"`
	Step          string         `json:"step"`
	ErrorMessage  string         `json:"error_message"`
	InstanceID    string         `json:"instance_id"`
	PublicIP      string         `json:"public_ip"`
	LoginUser     string         `json:"login_user"`
	LoginPassword string         `json:"login_password,omitempty"`
	Payload       map[string]any `json:"payload,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type EcsPreview struct {
	Account           map[string]any `json:"account"`
	RegionID          string         `json:"regionId"`
	ZoneID            string         `json:"zoneId"`
	InstanceType      string         `json:"instanceType"`
	InstanceName      string         `json:"instanceName"`
	OSKey             string         `json:"osKey"`
	OSLabel           string         `json:"osLabel"`
	ImageID           string         `json:"imageId"`
	LoginUser         string         `json:"loginUser"`
	LoginPort         int            `json:"loginPort"`
	ClientCIDR        string         `json:"clientCidrIp"`
	InternetBandwidth int            `json:"internetMaxBandwidthOut"`
	PublicIPMode      string         `json:"publicIpMode"`
	PublicIPModeLabel string         `json:"publicIpModeLabel"`
	SystemDisk        map[string]any `json:"systemDisk"`
	Network           map[string]any `json:"network"`
	Pricing           map[string]any `json:"pricing"`
	Warnings          []string       `json:"warnings"`
}
