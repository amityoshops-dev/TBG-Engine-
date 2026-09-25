//go:build integration

// Run with: go test -tags=integration ./tests/...
// Requires the engine running on localhost:8080 against a fresh docker-compose stack.
package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

const baseURL = "http://localhost:8080"

func TestPayoutSuccess(t *testing.T) {
	body := map[string]interface{}{
		"corporate_account": "00040310001928",
		"amount":            2500000.00,
		"currency":          "INR",
		"payment_rail":      "RTGS",
		"beneficiary_name":  "Larsen and Toubro Limited",
		"beneficiary_acct":  "98127391827",
		"beneficiary_ifsc":  "SBIN0000123",
		"reference_id":      "IT-TEST-001",
	}
	b, _ := json.Marshal(body)

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/cms/payout", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "IT-IDEMP-001")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var parsed map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&parsed)
	if parsed["status"] != "SETTLED" {
		t.Fatalf("expected status SETTLED, got %v", parsed["status"])
	}
	if parsed["utr"] == "" || parsed["utr"] == nil {
		t.Fatalf("expected a non-empty UTR in response")
	}
}

func TestPayoutIdempotencyReplay(t *testing.T) {
	body := map[string]interface{}{
		"corporate_account": "00040310001928",
		"amount":            100000.00,
		"reference_id":      "IT-TEST-002",
	}
	b, _ := json.Marshal(body)
	idempKey := "IT-IDEMP-002"

	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/cms/payout", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempKey)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
		if i == 1 && resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict on replay, got %d", resp.StatusCode)
		}
	}
}

func TestPayoutInsufficientLiquidity(t *testing.T) {
	body := map[string]interface{}{
		"corporate_account": "00040310001928",
		"amount":            999999999999.00,
		"reference_id":      "IT-TEST-003",
	}
	b, _ := json.Marshal(body)

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/cms/payout", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "IT-IDEMP-003")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity, got %d", resp.StatusCode)
	}
}
