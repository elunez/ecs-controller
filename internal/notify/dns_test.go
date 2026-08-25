package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Kori1c/ecs-controller/internal/cloud"
)

func TestSplitDNSName(t *testing.T) {
	tests := []struct {
		domain, root, record string
	}{
		{"service.example.com", "example.com", "service"},
		{"a.b.example.co.uk", "example.co.uk", "a.b"},
		{"example.com", "example.com", "@"},
	}
	for _, test := range tests {
		root, record, err := splitDNSName(test.domain)
		if err != nil || root != test.root || record != test.record {
			t.Fatalf("split %q: root=%q record=%q err=%v", test.domain, root, record, err)
		}
	}
}

func TestDNSPodUpdateModifiesExistingRecord(t *testing.T) {
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Header.Get("X-TC-Action")
		actions = append(actions, action)
		w.Header().Set("Content-Type", "application/json")
		if action == "DescribeRecordList" {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["Domain"] != "example.com" || payload["Subdomain"] != "service" || payload["RecordType"] != "A" {
				t.Fatalf("unexpected list payload: %#v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Response": map[string]any{"RecordList": []map[string]any{{"RecordId": 42, "Line": "默认"}}}})
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if action != "ModifyRecord" || payload["RecordId"] != float64(42) || payload["Value"] != "203.0.113.8" || payload["TTL"] != float64(300) {
			t.Fatalf("unexpected update: action=%s payload=%#v", action, payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Response": map[string]any{}})
	}))
	defer server.Close()

	client := NewDNSPodClient("secret-id", "secret-key")
	client.Endpoint = server.URL
	client.HTTPClient = server.Client()
	if err := client.Update(context.Background(), "example.com", "service", "203.0.113.8", 300); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0] != "DescribeRecordList" || actions[1] != "ModifyRecord" {
		t.Fatalf("unexpected actions: %#v", actions)
	}
}

func TestAliDNSUpdateCreatesAndModifiesRecords(t *testing.T) {
	for _, test := range []struct {
		name, recordID, expectedAction string
	}{
		{"create", "", "AddDomainRecord"},
		{"modify", "record-42", "UpdateDomainRecord"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var actions []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				action := r.URL.Query().Get("Action")
				actions = append(actions, action)
				w.Header().Set("Content-Type", "application/json")
				if action == "DescribeDomainRecords" {
					records := []map[string]any{}
					if test.recordID != "" {
						records = append(records, map[string]any{"RecordId": test.recordID, "RR": "service", "Type": "A"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"DomainRecords": map[string]any{"Record": records}})
					return
				}
				query, _ := url.ParseQuery(r.URL.RawQuery)
				if action != test.expectedAction || query.Get("RR") != "service" || query.Get("Value") != "203.0.113.9" || query.Get("TTL") != "600" {
					t.Fatalf("unexpected update query: %s", r.URL.RawQuery)
				}
				if test.recordID == "" && query.Get("DomainName") != "example.com" {
					t.Fatalf("missing domain name: %s", r.URL.RawQuery)
				}
				if test.recordID != "" && query.Get("RecordId") != test.recordID {
					t.Fatalf("missing record id: %s", r.URL.RawQuery)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{})
			}))
			defer server.Close()

			client := &AliDNSClient{RPC: &cloud.RPCClient{HTTPClient: server.Client(), Endpoint: server.URL + "/", Version: "2015-01-09", Product: "Alidns", AccessKey: "ak", Secret: "sk"}}
			if err := client.Update(context.Background(), "example.com", "service", "203.0.113.9", 600); err != nil {
				t.Fatal(err)
			}
			if len(actions) != 2 || actions[1] != test.expectedAction {
				t.Fatalf("unexpected actions: %#v", actions)
			}
		})
	}
}
