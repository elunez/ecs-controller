package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudflareUpdateDeleteAndAutoResolveZone(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/zones":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{{"id": "zone-1"}}})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{{"id": "record-1"}}})
		case r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			if r.Method != http.MethodPut {
				t.Fatalf("expected PUT for existing record, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		}
	}))
	defer server.Close()

	client := NewCloudflareClient("token", "", "example.com")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	if err := client.Update(context.Background(), "node.example.com", "203.0.113.10", false); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(context.Background(), "node.example.com"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 5 || !strings.HasPrefix(requests[0], "GET /zones") || !strings.Contains(requests[4], "record-1") {
		t.Fatalf("requests: %#v", requests)
	}
}

func TestCloudflareUpdateUsesConfiguredTTLUnlessProxied(t *testing.T) {
	var ttls []float64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		ttls = append(ttls, payload["ttl"].(float64))
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
	}))
	defer server.Close()
	client := NewCloudflareClient("token", "zone-1", "example.com")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	if err := client.UpdateWithTTL(context.Background(), "service.example.com", "203.0.113.10", 300, false); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateWithTTL(context.Background(), "service.example.com", "203.0.113.10", 300, true); err != nil {
		t.Fatal(err)
	}
	if len(ttls) != 2 || ttls[0] != 300 || ttls[1] != 1 {
		t.Fatalf("unexpected TTLs: %#v", ttls)
	}
}
