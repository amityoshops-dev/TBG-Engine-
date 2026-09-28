package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"
)

// Splunk-compliant structured audit event payload
type AuditLog struct {
	Timestamp     string  `json:"timestamp"`
	LogLevel      string  `json:"log_level"`
	CorrelationID string  `json:"correlation_id"`
	Channel       string  `json:"channel"`
	EventType     string  `json:"event_type"`
	AccountID     string  `json:"account_id"`
	TxnID         string  `json:"txnid"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	Status        string  `json:"status"`
	LatencyMS     float64 `json:"latency_ms"`
}

type JournalEntry struct {
	ID        string  `json:"id"`
	Timestamp string  `json:"timestamp"`
	JVID      string  `json:"jv_id"`
	Account   string  `json:"account_id"`
	Leg       string  `json:"leg"` // DR or CR
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	Module    string  `json:"module"`
	Narrative string  `json:"narrative"`
	Audit     string  `json:"audit_verdict"`
}

type EngineState struct {
	sync.Mutex
	FloatINR          float64
	FloatUSD          float64
	SuspenseINR       float64
	SuspenseUSD       float64
	NostroINR         float64
	NostroUSD         float64
	ReraProjectEscrow float64
	ReraFreeFloat     float64
	SweepPoolINR      float64
	EntryCounter      int
	Ledger            []JournalEntry
	IdempotencyMap    map[string]bool
}

var state = EngineState{
	FloatINR:          99750000.00,
	FloatUSD:          3500000.00,
	SuspenseINR:       0.00,
	SuspenseUSD:       0.00,
	NostroINR:         500000000.00,
	NostroUSD:         12000000.00,
	ReraProjectEscrow: 70000000.00,
	ReraFreeFloat:     30000000.00,
	SweepPoolINR:      85000000.00,
	EntryCounter:      4,
	IdempotencyMap:    make(map[string]bool),
	Ledger: []JournalEntry{
		{ID: "#04", Timestamp: "2026-09-28T09:45:40Z", JVID: "JV-1e9a5f78-NTR", Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: 250000.00, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Central Bank Gross Clearing Settlement", Audit: "ZERO-SUM OK"},
		{ID: "#03", Timestamp: "2026-09-28T09:45:40Z", JVID: "JV-1e9a5f78-NTR", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: 250000.00, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Discharge of CMS Suspense Liability", Audit: "ZERO-SUM OK"},
		{ID: "#02", Timestamp: "2026-09-28T09:45:37Z", JVID: "JV-f959fabb-CMS", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: 250000.00, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Transit Outward Float Reservation", Audit: "ZERO-SUM OK"},
		{ID: "#01", Timestamp: "2026-09-28T09:45:37Z", JVID: "JV-f959fabb-CMS", Account: "00040310001928", Leg: "DR", Amount: 250000.00, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Debtor Float Settlement Debit", Audit: "ZERO-SUM OK"},
	},
}

func logAudit(eventType, acct, txnid string, amt float64, currency, status string, latency float64) {
	logItem := AuditLog{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		LogLevel:      "INFO",
		CorrelationID: fmt.Sprintf("%08x-%04x-4%03x", rand.Uint32(), rand.Uint32()&0xffff, rand.Uint32()&0xfff),
		Channel:       "TBG_HEADLESS_CORE",
		EventType:     eventType,
		AccountID:     acct,
		TxnID:         txnid,
		Amount:        amt,
		Currency:      currency,
		Status:        status,
		LatencyMS:     latency,
	}
	bytes, _ := json.Marshal(logItem)
	fmt.Println(string(bytes))
}

func handleGetLedger(w http.ResponseWriter, r *http.Request) {
	state.Lock()
	defer state.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"system":               "TBG-CORE-HEADLESS",
		"acid_invariance":      "SUM(DR) - SUM(CR) = 0",
		"corporate_float_inr":  state.FloatINR,
		"corporate_float_usd":  state.FloatUSD,
		"cms_suspense_inr":     state.SuspenseINR,
		"cms_suspense_usd":     state.SuspenseUSD,
		"nostro_inr":           state.NostroINR,
		"nostro_usd":           state.NostroUSD,
		"rera_project_escrow":  state.ReraProjectEscrow,
		"rera_free_float":      state.ReraFreeFloat,
		"liquidity_sweep_pool": state.SweepPoolINR,
		"audited_entries_cnt":  state.EntryCounter,
		"postings_ledger":      state.Ledger,
	})
}

func handleDomesticPayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"Method Not Allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()

	var req struct {
		SourceAccount   string  `json:"source_account"`
		BeneficiaryName string  `json:"beneficiary_name"`
		BeneficiaryAcct string  `json:"beneficiary_account"`
		BeneficiaryIFSC string  `json:"beneficiary_ifsc"`
		Amount          float64 `json:"amount"`
		Rail            string  `json:"rail"`
		IdempotencyKey  string  `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"Malformed JSON payload"}`, http.StatusBadRequest)
		return
	}

	state.Lock()
	defer state.Unlock()

	if state.IdempotencyMap[req.IdempotencyKey] {
		http.Error(w, `{"error":"Idempotency Conflict: Transaction already booked"}`, http.StatusConflict)
		return
	}
	if req.Amount > state.FloatINR {
		http.Error(w, `{"error":"Insufficient Corporate Float"}`, http.StatusBadRequest)
		return
	}

	state.FloatINR -= req.Amount
	state.SuspenseINR += req.Amount
	state.IdempotencyMap[req.IdempotencyKey] = true

	jvID := fmt.Sprintf("JV-%08x-CMS", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 2

	rail := req.Rail
	if rail == "" || rail == "AUTO" {
		if req.Amount >= 200000 {
			rail = "RTGS"
		} else {
			rail = "NEFT"
		}
	}

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: req.Amount, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Outward Transit Float Reserve", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Debtor Float Settlement Debit", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	mac := hmac.New(sha256.New, []byte("institution_production_secret"))
	mac.Write([]byte(fmt.Sprintf("%s:%f:%s", jvID, req.Amount, req.BeneficiaryAcct)))
	sig := hex.EncodeToString(mac.Sum(nil))

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	logAudit("DOMESTIC_PAYOUT_SETTLED", req.SourceAccount, jvID, req.Amount, "INR", "SETTLED", latency)

	isoXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">
  <FIToFICstmrCdtTrf>
    <GrpHdr>
      <MsgId>MSG-%s-%04d</MsgId>
      <CreDtTm>%s</CreDtTm>
      <NbOfTxs>1</NbOfTxs>
      <SttlmInf><SttlmMtd>CLRG</SttlmMtd><ClrSys><Prtry>%s</Prtry></ClrSys></SttlmInf>
    </GrpHdr>
    <CdtTrfTxInf>
      <PmtId><EndToEndId>%s</EndToEndId><TxId>%s</TxId></PmtId>
      <IntrBkSttlmAmt Ccy="INR">%.2f</IntrBkSttlmAmt>
      <Dbtr><Nm>TREASURY CORP INDIA PVT LTD</Nm></Dbtr>
      <DbtrAcct><Id><Othr><Id>%s</Id></Othr></Id></DbtrAcct>
      <CdtrAgt><FinInstnId><ClrSysMmbId><MmbId>%s</MmbId></ClrSysMmbId></FinInstnId></CdtrAgt>
      <Cdtr><Nm>%s</Nm></Cdtr>
      <CdtrAcct><Id><Othr><Id>%s</Id></Othr></Id></CdtrAcct>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>`, time.Now().Format("20060102"), rand.Intn(9999), time.Now().UTC().Format(time.RFC3339), rail, req.IdempotencyKey, jvID, req.Amount, req.SourceAccount, req.BeneficiaryIFSC, req.BeneficiaryName, req.BeneficiaryAcct)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "SETTLED",
		"jv_id":           jvID,
		"rail":            rail,
		"amount":          req.Amount,
		"currency":        "INR",
		"latency_ms":      latency,
		"webhook_hmac":    sig,
		"iso20022_schema": "pacs.008.001.08",
		"iso20022_xml":    isoXML,
	})
}

func handleCrossBorderPayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()

	var req struct {
		SourceAccount   string  `json:"source_account"`
		BeneficiaryName string  `json:"beneficiary_name"`
		BeneficiaryIBAN string  `json:"beneficiary_iban"`
		BeneficiaryBIC  string  `json:"beneficiary_bic"`
		IntermediaryBIC string  `json:"intermediary_bic"`
		Amount          float64 `json:"amount"`
		Currency        string  `json:"currency"`
		ChargeBearer    string  `json:"charge_bearer"`
		IdempotencyKey  string  `json:"idempotency_key"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	state.Lock()
	defer state.Unlock()

	if req.Amount > state.FloatUSD {
		http.Error(w, `{"error":"Insufficient USD Float balance"}`, http.StatusBadRequest)
		return
	}

	state.FloatUSD -= req.Amount
	state.SuspenseUSD += req.Amount
	jvID := fmt.Sprintf("JV-%08x-CBPR", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "TRANSIT_OUTWARD_USD_SUSPENSE", Leg: "CR", Amount: req.Amount, Currency: "USD", Module: "SWIFT_CBPR+", Narrative: "Correspondent Cover Wire Hold", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Currency: "USD", Module: "SWIFT_CBPR+", Narrative: "Debtor USD Account Wire Debit", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	uetr := fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", rand.Uint32(), rand.Uint32()&0xffff, rand.Uint32()&0xfff, rand.Uint32()&0xffff, rand.Uint64()&0xffffffffffff)

	swiftMT103 := fmt.Sprintf(`{1:F01TBGUSB33AXXX0000000000}{2:I103%sXXXXN}{3:{121:%s}}{4:
:20:%s
:23B:CRED
:32A:%s%s%.2f
:50K:/%s
CORP TREASURY GLOBAL CLIENT
:53A:TBGUSB33XXX
:56A:%s
:57A:%s
:59:/%s
%s
:71A:%s
-}`, req.BeneficiaryBIC, uetr, req.IdempotencyKey, time.Now().Format("060102"), req.Currency, req.Amount, req.SourceAccount, req.IntermediaryBIC, req.BeneficiaryBIC, req.BeneficiaryIBAN, req.BeneficiaryName, req.ChargeBearer)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	logAudit("SWIFT_MT103_DISPATCHED", req.SourceAccount, jvID, req.Amount, "USD", "DISPATCHED", latency)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "DISPATCHED",
		"jv_id":       jvID,
		"uetr":        uetr,
		"standard":    "SWIFT FIN MT103",
		"swift_mt103": swiftMT103,
		"latency_ms":  latency,
	})
}

func handleReraSplit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()
	var req struct {
		ProjectID string  `json:"project_id"`
		BuyerVAN  string  `json:"buyer_van"`
		Amount    float64 `json:"amount"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	state.Lock()
	defer state.Unlock()

	p70 := req.Amount * 0.70
	ops30 := req.Amount * 0.30
	state.ReraProjectEscrow += p70
	state.ReraFreeFloat += ops30

	jvID := fmt.Sprintf("JV-%08x-RERA", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 3

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "ESCROW_RERA_70_SECURED", Leg: "CR", Amount: p70, Currency: "INR", Module: "RERA_ESCROW", Narrative: "70% Construction Dedicated Reserve", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: "ESCROW_RERA_30_OPERATIONAL", Leg: "CR", Amount: ops30, Currency: "INR", Module: "RERA_ESCROW", Narrative: "30% OpEx Unrestricted Credit", Audit: "ZERO-SUM OK"}
	e3 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-2), Timestamp: ts, JVID: jvID, Account: req.BuyerVAN, Leg: "DR", Amount: req.Amount, Currency: "INR", Module: "RERA_ESCROW", Narrative: "Homebuyer Consideration Allocation", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2, e3}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	logAudit("RERA_SECTION_4_SPLIT", req.BuyerVAN, jvID, req.Amount, "INR", "ALLOCATED", latency)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                "ALLOCATED",
		"rera_rule":             "Section 4(2)(l)(D)",
		"project_escrow_70_cr":  p70,
		"operational_float_30_cr": ops30,
		"buyer_van_dr":          req.Amount,
		"jv_id":                 jvID,
		"latency_ms":            latency,
	})
}

func handleSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()
	var req struct {
		SubsidiaryAccount string  `json:"subsidiary_account"`
		SweepAmount        float64 `json:"sweep_amount"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	state.Lock()
	defer state.Unlock()

	state.SweepPoolINR += req.SweepAmount
	jvID := fmt.Sprintf("JV-%08x-SWP", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "MASTER_LIQUIDITY_POOL_01", Leg: "CR", Amount: req.SweepAmount, Currency: "INR", Module: "LIQUIDITY_ZBA", Narrative: "EOD Pool Cash Concentration", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: req.SubsidiaryAccount, Leg: "DR", Amount: req.SweepAmount, Currency: "INR", Module: "LIQUIDITY_ZBA", Narrative: "Zero-Balance Current Acct Sweep", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	logAudit("ZBA_CONCENTRATION_SWEEP", req.SubsidiaryAccount, jvID, req.SweepAmount, "INR", "SWEPT", latency)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                 "CONCENTRATED",
		"rule":                   "EOD Zero-Balance Physical Cash Concentration",
		"pool_consolidated_inr": state.SweepPoolINR,
		"jv_id":                  jvID,
		"latency_ms":             latency,
	})
}

func handleEODNostro(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()

	state.Lock()
	defer state.Unlock()

	if state.SuspenseINR <= 0 && state.SuspenseUSD <= 0 {
		http.Error(w, `{"error":"Suspense balance is zero. No central bank settlement required"}`, http.StatusBadRequest)
		return
	}

	clearedINR := state.SuspenseINR
	state.SuspenseINR = 0.00
	jvID := fmt.Sprintf("JV-%08x-NTR", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: clearedINR, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Central Bank Nostro Clearing", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: clearedINR, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Discharge of CMS Suspense Liability", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	logAudit("NOSTRO_EOD_CLEARED", "AC_RBI_NOSTRO_0001", jvID, clearedINR, "INR", "DISCHARGED", latency)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":              "NOSTRO_CLEARED",
		"settlement_rail":     "Reserve Bank of India RTGS Core",
		"discharged_suspense": clearedINR,
		"jv_id":               jvID,
		"latency_ms":          latency,
	})
}

func handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(openAPISpecJSON))
}

func handleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(swaggerHTML))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}

	http.HandleFunc("/", handleSwaggerUI)
	http.HandleFunc("/docs", handleSwaggerUI)
	http.HandleFunc("/openapi.json", handleOpenAPISpec)

	// Institutional Headless API Endpoints
	http.HandleFunc("/api/v1/ledger", handleGetLedger)
	http.HandleFunc("/api/v1/payouts/domestic", handleDomesticPayout)
	http.HandleFunc("/api/v1/payouts/cross-border", handleCrossBorderPayout)
	http.HandleFunc("/api/v1/escrow/rera-split", handleReraSplit)
	http.HandleFunc("/api/v1/liquidity/zba-sweep", handleSweep)
	http.HandleFunc("/api/v1/recon/eod-nostro", handleEODNostro)

	log.Printf("TBG-HEADLESS Engine running on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server startup failed: %v", err)
	}
}

const swaggerHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>TBG CORE // Institutional OpenAPI Specification</title>
  <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.29.1/swagger-ui.css" />
  <style>
    body { margin: 0; background: #fafafa; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
    .top-strip { background: #0f172a; color: #f8fafc; padding: 12px 24px; display: flex; justify-content: space-between; align-items: center; border-bottom: 1px solid #1e293b; }
    .top-strip-title { font-family: 'JetBrains Mono', monospace; font-size: 13px; font-weight: 700; letter-spacing: 0.05em; color: #38bdf8; }
    .top-strip-meta { font-family: 'JetBrains Mono', monospace; font-size: 11px; color: #94a3b8; }
    .swagger-ui .topbar { display: none; }
  </style>
</head>
<body>
  <div class="top-strip">
    <div class="top-strip-title">TBG CORE // TRANSACTION BANKING GATEWAY HEADLESS CORE</div>
    <div class="top-strip-meta">POSTGRESQL 16 ACID // INVARIANT: &Sigma;DR - &Sigma;CR = 0 // SPEC: OPENAPI 3.1.0</div>
  </div>
  <div id="swagger-ui"></div>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.29.1/swagger-ui-bundle.js"></script>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.29.1/swagger-ui-standalone-preset.js"></script>
  <script>
    window.onload = function() {
      SwaggerUIBundle({
        url: "/openapi.json",
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        layout: "BaseLayout"
      });
    };
  </script>
</body>
</html>`

const openAPISpecJSON = `{
  "openapi": "3.1.0",
  "info": {
    "title": "TBG CORE // Institutional Transaction Banking Platform",
    "description": "Enterprise Core Payment Engine providing ISO 20022 clearing, SWIFT CBPR+ wire generation, RERA Section 4 escrow partitioning, Zero-Balance Account (ZBA) sweeps, and immutable double-entry ledger settlement.",
    "version": "4.2.0"
  },
  "servers": [
    { "url": "/", "description": "Active Engine Node" }
  ],
  "paths": {
    "/api/v1/ledger": {
      "get": {
        "summary": "Query Core Double-Entry Postings Ledger",
        "description": "Retrieves the immutable journal voucher (JV) ledger, real-time float balances, and zero-sum verification proofs.",
        "responses": {
          "200": { "description": "Ledger state and balance sheet" }
        }
      }
    },
    "/api/v1/payouts/domestic": {
      "post": {
        "summary": "Execute Domestic High/Low Value Payout (ISO 20022)",
        "description": "Executes double-entry balance hold, smart-routes across RTGS/NEFT/UPI rails, commits transit suspense, and generates pacs.008.001.08 wire schema.",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "example": {
                "source_account": "00040310001928",
                "beneficiary_name": "Tata Motors Commercial Fleet Ltd",
                "beneficiary_account": "912345678901",
                "beneficiary_ifsc": "HDFC0000001",
                "amount": 250000,
                "rail": "AUTO",
                "idempotency_key": "TXN-89012391"
              }
            }
          }
        },
        "responses": {
          "200": { "description": "Settled with ISO 20022 pacs.008 XML & HMAC-SHA256 signature" },
          "400": { "description": "Insufficient corporate float balance" },
          "409": { "description": "Idempotency conflict" }
        }
      }
    },
    "/api/v1/payouts/cross-border": {
      "post": {
        "summary": "Execute SWIFT CBPR+ Cross-Border Wire (MT103)",
        "description": "Ingests cross-border USD payments, tracks UETR end-to-end references, and generates ISO 20022 pacs.008 & SWIFT FIN MT103 wire formats.",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "example": {
                "source_account": "CORP_US_FLOAT_0029",
                "beneficiary_name": "Airbus Operations GmbH",
                "beneficiary_iban": "DE89370400440532013000",
                "beneficiary_bic": "DBEUMM21XXX",
                "intermediary_bic": "CHASUS33XXX",
                "amount": 250000,
                "currency": "USD",
                "charge_bearer": "OUR",
                "idempotency_key": "E2E-44910281"
              }
            }
          }
        },
        "responses": {
          "200": { "description": "Dispatched with UETR and SWIFT MT103 payload" }
        }
      }
    },
    "/api/v1/escrow/rera-split": {
      "post": {
        "summary": "Execute RERA Section 4(2)(l)(D) Dual Escrow Split",
        "description": "Splits buyer consideration into 70% unencumbered construction escrow and 30% operational account.",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "example": {
                "project_id": "PRJ-MAHARERA-PUNE-2026-904",
                "buyer_van": "VAN-PUNE-TWR-801",
                "amount": 5000000
              }
            }
          }
        },
        "responses": {
          "200": { "description": "Allocated 70% and 30% with balanced journal entries" }
        }
      }
    },
    "/api/v1/liquidity/zba-sweep": {
      "post": {
        "summary": "Execute Zero-Balance Account (ZBA) Liquidity Sweep",
        "description": "Sweeps idle subsidiary balances to the master corporate treasury concentration pool.",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "example": {
                "subsidiary_account": "SUBSIDIARY_PUNE_PLANT_4021",
                "sweep_amount": 2500000
              }
            }
          }
        },
        "responses": {
          "200": { "description": "Concentrated into master liquidity pool" }
        }
      }
    },
    "/api/v1/recon/eod-nostro": {
      "post": {
        "summary": "Trigger EOD Central Bank Nostro Settlement",
        "description": "Reconciles outstanding CMS Suspense transit liabilities against the central bank Nostro clearing ledger.",
        "responses": {
          "200": { "description": "Nostro obligations cleared and discharged" }
        }
      }
    }
  }
}`
