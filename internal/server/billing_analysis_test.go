package server

import (
	"testing"

	"github.com/Kori1c/ecs-controller/internal/cloud"
)

func TestSummarizeBillingCategories(t *testing.T) {
	items := summarizeBillingCategories([]cloud.BillingDetail{
		{ProductName: "Elastic Compute Service", ProductCode: "ecs", Amount: 4},
		{ProductName: "Cloud Disk", BillingItem: "ESSD", Amount: 2},
		{ProductName: "Elastic IP Address", ProductCode: "eip", Amount: 1},
		{ProductName: "Cloud Data Transfer", BillingItem: "traffic", Amount: 3},
		{ProductName: "Support", Amount: 0.5},
	})
	amounts := make(map[string]float64)
	for _, item := range items {
		amounts[item.Key] = item.Amount
	}
	for key, expected := range map[string]float64{"instance": 4, "disk": 2, "eip": 1, "network": 3, "other": 0.5} {
		if amounts[key] != expected {
			t.Fatalf("category %s=%v, want %v (%#v)", key, amounts[key], expected, items)
		}
	}
}
