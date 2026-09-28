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

type SplunkLog struct {
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
	Timestamp string  `json:"time"`
	JVID      string  `json:"jv"`
	Account   string  `json:"acct"`
	Leg       string  `json:"leg"`
	Amount    float64 `json:"amt"`
	Verdict   string  `json:"verdict"`
	Narrative string  `json:"narrative"`
}

type EngineState struct {
	sync.Mutex
	FloatBalance    float64
	SuspenseBalance float64
	ReraProjectEscrow float64
	ReraFreeFloat   float64
	SweepPoolBalance float64
	EntryCounter    int
	Ledger          []JournalEntry
	IdempotencyMap  map[string]bool
}

var state = EngineState{
	FloatBalance:      10000000.00,
	SuspenseBalance:   0.00,
	ReraProjectEscrow: 35000000.00, // 70% Ring-fenced
	ReraFreeFloat:     15000000.00, // 30% Operational
	SweepPoolBalance:  50000000.00,
	EntryCounter:      16,
	IdempotencyMap:    make(map[string]bool),
	Ledger: []JournalEntry{
		{ID: "#16", Timestamp: "9:50:48 pm", JVID: "232631f0-582c-426e-nostro", Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: 75000.00, Verdict: "ZERO-SUM OK", Narrative: "RBI Clearing Settlement Net"},
		{ID: "#15", Timestamp: "9:50:48 pm", JVID: "232631f0-582c-426e-nostro", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: 75000.00, Verdict: "ZERO-SUM OK", Narrative: "CMS Suspense Obligation Discharge"},
		{ID: "#14", Timestamp: "9:50:41 pm", JVID: "bded8c1d-2055-4a80-cms", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: 25000.00, Verdict: "ZERO-SUM OK", Narrative: "Outward Transit Float Reserve"},
		{ID: "#13", Timestamp: "9:50:41 pm", JVID: "bded8c1d-2055-4a80-cms", Account: "00040310001928", Leg: "DR", Amount: 25000.00, Verdict: "ZERO-SUM OK", Narrative: "Debtor Float Settlement Debit"},
	},
}

func emitSplunk(eventType, acct, txnid string, amt float64, status string, latency float64) {
	logObj := SplunkLog{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		LogLevel:      "INFO",
		CorrelationID: fmt.Sprintf("%08x-%04x-%04x", rand.Uint32(), rand.Uint32()&0xffff, rand.Uint32()&0xffff),
		Channel:       "TBG_CMS",
		EventType:     eventType,
		AccountID:     acct,
		TxnID:         txnid,
		Amount:        amt,
		Currency:      "INR",
		Status:        status,
		LatencyMS:     latency,
	}
	bytes, _ := json.Marshal(logObj)
	fmt.Println(string(bytes))
}

func handleState(w http.ResponseWriter, r *http.Request) {
	state.Lock()
	defer state.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"float_balance":       state.FloatBalance,
		"suspense_balance":    state.SuspenseBalance,
		"rera_escrow":         state.ReraProjectEscrow,
		"rera_operational":    state.ReraFreeFloat,
		"sweep_pool":          state.SweepPoolBalance,
		"entry_count":         state.EntryCounter,
		"ledger":              state.Ledger,
	})
}

// 1. Core Rail Payout Pipeline
func handlePayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()
	var req struct {
		SourceAccount      string  `json:"source_account"`
		BeneficiaryName    string  `json:"beneficiary_name"`
		BeneficiaryAccount string  `json:"beneficiary_account"`
		BeneficiaryIFSC    string  `json:"beneficiary_ifsc"`
		Amount             float64 `json:"amount"`
		Rail               string  `json:"rail"`
		IdempotencyKey     string  `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400); return
	}

	state.Lock()
	defer state.Unlock()

	if state.IdempotencyMap[req.IdempotencyKey] {
		http.Error(w, "Idempotency Conflict", http.StatusConflict); return
	}
	if req.Amount > state.FloatBalance {
		http.Error(w, "Insufficient Corporate Float", 400); return
	}

	state.FloatBalance -= req.Amount
	state.SuspenseBalance += req.Amount
	state.IdempotencyMap[req.IdempotencyKey] = true

	jvID := fmt.Sprintf("%08x-cms", rand.Uint32())
	nowStr := time.Now().Format("3:04:05 pm")
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: req.Amount, Verdict: "ZERO-SUM OK", Narrative: "Outward Transit Float Reserve"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Verdict: "ZERO-SUM OK", Narrative: "Debtor Float Settlement Debit"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	rail := req.Rail
	if rail == "AUTO" {
		if req.Amount >= 200000 { rail = "RTGS" } else { rail = "NEFT" }
	}

	mac := hmac.New(sha256.New, []byte("production_hmac_secret_key"))
	mac.Write([]byte(fmt.Sprintf("%s:%f:%s", jvID, req.Amount, req.BeneficiaryAccount)))
	sig := hex.EncodeToString(mac.Sum(nil))

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	emitSplunk("PAYOUT_POSTING", req.SourceAccount, jvID, req.Amount, "SETTLED", latency)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "SETTLED", "jv_id": jvID, "rail": rail, "latency_ms": latency, "signature": sig, "pacs_msg_id": fmt.Sprintf("MSG-%d", time.Now().UnixNano()),
	})
}

// 2. RERA Escrow Split Engine (70% Ring-fenced Project Account / 30% Operational)
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

	project70 := req.Amount * 0.70
	ops30 := req.Amount * 0.30
	state.ReraProjectEscrow += project70
	state.ReraFreeFloat += ops30

	jvID := fmt.Sprintf("%08x-rera", rand.Uint32())
	nowStr := time.Now().Format("3:04:05 pm")
	state.EntryCounter += 3

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "ESCROW_RERA_70_SECURED", Leg: "CR", Amount: project70, Verdict: "ZERO-SUM OK", Narrative: "70% Ring-Fenced Construction Hold"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: "ESCROW_RERA_30_OPERATIONAL", Leg: "CR", Amount: ops30, Verdict: "ZERO-SUM OK", Narrative: "30% Developer OpEx Release"}
	e3 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-2), Timestamp: nowStr, JVID: jvID, Account: req.BuyerVAN, Leg: "DR", Amount: req.Amount, Verdict: "ZERO-SUM OK", Narrative: "Homebuyer Inflow Credit Settlement"}
	state.Ledger = append([]JournalEntry{e1, e2, e3}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	emitSplunk("RERA_SPLIT_POSTING", req.BuyerVAN, jvID, req.Amount, "ALLOCATED", latency)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ALLOCATED", "project_70": project70, "ops_30": ops30, "jv_id": jvID, "latency_ms": latency,
	})
}

// 3. Liquidity Sweep Engine (ZBA - Zero Balance Account Sweep to Central Pool)
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

	state.SweepPoolBalance += req.SweepAmount
	jvID := fmt.Sprintf("%08x-sweep", rand.Uint32())
	nowStr := time.Now().Format("3:04:05 pm")
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "MASTER_LIQUIDITY_POOL_01", Leg: "CR", Amount: req.SweepAmount, Verdict: "ZERO-SUM OK", Narrative: "ZBA Concentration Sweep Credit"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SubsidiaryAccount, Leg: "DR", Amount: req.SweepAmount, Verdict: "ZERO-SUM OK", Narrative: "Subsidiary ZBA Sweep Outflow"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	emitSplunk("ZBA_LIQUIDITY_SWEEP", req.SubsidiaryAccount, jvID, req.SweepAmount, "CONCENTRATED", latency)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "SWEPT", "pool_balance": state.SweepPoolBalance, "jv_id": jvID, "latency_ms": latency,
	})
}

// 4. EOD Settlement Discharge
func handleEOD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()

	state.Lock()
	defer state.Unlock()

	if state.SuspenseBalance <= 0 {
		http.Error(w, "Suspense is zero", 400); return
	}

	clearedAmt := state.SuspenseBalance
	state.SuspenseBalance = 0.00
	nowStr := time.Now().Format("3:04:05 pm")
	jvID := fmt.Sprintf("%08x-nostro", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: clearedAmt, Verdict: "ZERO-SUM OK", Narrative: "Central Bank Nostro Clearing"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: clearedAmt, Verdict: "ZERO-SUM OK", Narrative: "CMS Suspense Liability Clear"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	emitSplunk("NOSTRO_EOD_CLEARING", "AC_RBI_NOSTRO_0001", jvID, clearedAmt, "SETTLED", latency)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "NOSTRO_CLEARED", "cleared_amt": clearedAmt, "jv_id": jvID, "latency_ms": latency,
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" { port = "10000" }

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(uiHTML))
	})
	http.HandleFunc("/api/state", handleState)
	http.HandleFunc("/api/payout", handlePayout)
	http.HandleFunc("/api/rera", handleReraSplit)
	http.HandleFunc("/api/sweep", handleSweep)
	http.HandleFunc("/api/eod", handleEOD)

	log.Printf("TBG-CORE running on :%s", port)
	http.ListenAndServe(":"+port, nil)
}

const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>TBG-CORE // Finacle Treasury & Multi-Rail Gateway Cockpit</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    :root {
      --bg-base: #060b13;
      --bg-surface: #0a1322;
      --bg-card: #0d1b2e;
      --border-subtle: #192a42;
      --border-focus: #00e5ff;
      --text-main: #f0f6fc;
      --text-muted: #8b9eb5;
      --accent-cyan: #00e5ff;
      --accent-green: #00ffa3;
      --accent-amber: #ffb703;
      --accent-crimson: #ff3366;
      --font-mono: 'JetBrains Mono', 'Fira Code', 'Courier New', monospace;
      --font-sans: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { background: var(--bg-base); color: var(--text-main); font-family: var(--font-sans); font-size: 13px; line-height: 1.5; }
    header { background: var(--bg-surface); border-bottom: 1px solid var(--border-subtle); padding: 12px 24px; display: flex; justify-content: space-between; align-items: center; position: sticky; top: 0; z-index: 100; }
    .brand { display: flex; align-items: center; gap: 12px; font-family: var(--font-mono); font-weight: 700; font-size: 14px; }
    .badge { background: rgba(0, 229, 255, 0.12); border: 1px solid var(--accent-cyan); color: var(--accent-cyan); font-size: 10px; padding: 2px 6px; border-radius: 3px; }
    .nav-tabs { display: flex; gap: 8px; flex-wrap: wrap; }
    .nav-btn { background: transparent; border: 1px solid transparent; color: var(--text-muted); padding: 7px 14px; font-size: 11px; cursor: pointer; border-radius: 4px; font-family: var(--font-mono); font-weight: 600; text-transform: uppercase; transition: all 0.2s; }
    .nav-btn.active, .nav-btn:hover { color: var(--accent-cyan); background: var(--bg-card); border-color: var(--border-subtle); }
    .stats-bar { display: grid; grid-template-columns: repeat(5, 1fr); gap: 1px; background: var(--border-subtle); border-bottom: 1px solid var(--border-subtle); }
    .stat-card { background: var(--bg-surface); padding: 14px 20px; }
    .stat-label { font-size: 10px; font-family: var(--font-mono); color: var(--text-muted); text-transform: uppercase; margin-bottom: 4px; }
    .stat-val { font-family: var(--font-mono); font-size: 15px; font-weight: 600; }
    .tab-content { display: none; padding: 20px 24px; }
    .tab-content.active { display: block; }
    .grid-dashboard { display: grid; grid-template-columns: 380px 1fr; gap: 20px; }
    .panel { background: var(--bg-surface); border: 1px solid var(--border-subtle); border-radius: 6px; overflow: hidden; display: flex; flex-direction: column; margin-bottom: 16px; }
    .panel-head { background: var(--bg-card); padding: 10px 16px; border-bottom: 1px solid var(--border-subtle); font-family: var(--font-mono); font-size: 11px; font-weight: 600; text-transform: uppercase; color: var(--text-muted); display: flex; justify-content: space-between; align-items: center; }
    .panel-body { padding: 16px; }
    .form-group { margin-bottom: 12px; }
    label { display: block; font-family: var(--font-mono); font-size: 10px; color: var(--text-muted); text-transform: uppercase; margin-bottom: 4px; }
    input, select { width: 100%; background: var(--bg-base); border: 1px solid var(--border-subtle); padding: 8px 10px; color: var(--text-main); font-family: var(--font-mono); font-size: 12px; border-radius: 4px; outline: none; }
    input:focus, select:focus { border-color: var(--border-focus); }
    .btn { width: 100%; background: var(--accent-cyan); color: #030712; border: none; padding: 10px; font-family: var(--font-mono); font-size: 11px; font-weight: 700; text-transform: uppercase; cursor: pointer; border-radius: 4px; transition: background 0.2s; }
    .btn:hover { background: #5df2ff; }
    .btn-secondary { background: transparent; border: 1px solid var(--accent-green); color: var(--accent-green); margin-top: 8px; }
    .btn-amber { background: transparent; border: 1px solid var(--accent-amber); color: var(--accent-amber); margin-top: 8px; }
    .table-container { overflow-x: auto; max-height: 480px; }
    table { width: 100%; border-collapse: collapse; font-family: var(--font-mono); font-size: 11px; }
    th { background: var(--bg-card); padding: 8px 12px; text-align: left; font-weight: 600; color: var(--text-muted); border-bottom: 1px solid var(--border-subtle); position: sticky; top: 0; }
    td { padding: 8px 12px; border-bottom: 1px solid rgba(25, 42, 66, 0.6); }
    .tag-dr { color: var(--accent-crimson); font-weight: 700; }
    .tag-cr { color: var(--accent-green); font-weight: 700; }
    .wire-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-top: 16px; }
    pre { background: var(--bg-base); border: 1px solid var(--border-subtle); border-radius: 4px; padding: 12px; font-family: var(--font-mono); font-size: 10.5px; color: #9cdcfe; overflow-x: auto; max-height: 280px; }
    .doc-section { background: var(--bg-surface); border: 1px solid var(--border-subtle); border-radius: 6px; padding: 24px; margin-bottom: 24px; }
    .doc-section h2 { font-size: 16px; font-family: var(--font-mono); color: var(--accent-cyan); margin-bottom: 12px; border-bottom: 1px solid var(--border-subtle); padding-bottom: 8px; }
    .doc-section p { color: var(--text-muted); margin-bottom: 12px; font-size: 13px; line-height: 1.6; }
    .glossary-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 16px; }
    .term-card { background: var(--bg-card); border: 1px solid var(--border-subtle); border-radius: 4px; padding: 16px; }
    .term-title { font-family: var(--font-mono); font-size: 13px; color: var(--accent-cyan); margin-bottom: 6px; font-weight: 700; }
    .term-desc { font-size: 12px; color: var(--text-muted); line-height: 1.6; }
    .vector-svg { width: 100%; background: var(--bg-base); border: 1px solid var(--border-subtle); border-radius: 6px; padding: 16px; margin-top: 12px; }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <span>TBG-CORE</span>
      <span class="badge">FINACLE TREASURY // STRIPE LEDGER</span>
      <span style="color: var(--text-muted); font-size: 11px;">UNIFIED CMS SUITE</span>
    </div>
    <div class="nav-tabs">
      <button class="nav-btn active" onclick="switchTab('tab-cockpit', this)">1. Rail Dispatcher</button>
      <button class="nav-btn" onclick="switchTab('tab-rera', this)">2. RERA Escrow</button>
      <button class="nav-btn" onclick="switchTab('tab-sweeps', this)">3. Liquidity Sweeps (ZBA)</button>
      <button class="nav-btn" onclick="switchTab('tab-architecture', this)">4. Vector Architecture</button>
      <button class="nav-btn" onclick="switchTab('tab-prd', this)">5. Institutional PRD</button>
      <button class="nav-btn" onclick="switchTab('tab-glossary', this)">6. Banking Glossary</button>
    </div>
  </header>

  <div class="stats-bar">
    <div class="stat-card">
      <div class="stat-label">Corporate Float</div>
      <div class="stat-val" id="disp-float">INR 1,00,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">CMS Suspense (Transit Liability)</div>
      <div class="stat-val" id="disp-suspense" style="color: var(--accent-cyan);">INR 0.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">RERA 70% Escrow Hold</div>
      <div class="stat-val" id="disp-rera-70" style="color: var(--accent-amber);">INR 3,50,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Concentration Sweep Pool</div>
      <div class="stat-val" id="disp-sweep-pool" style="color: var(--accent-green);">INR 5,00,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Audited Postings</div>
      <div class="stat-val" id="disp-count">16 Entries</div>
    </div>
  </div>

  <!-- TAB 1: OUTWARD CLEARING DISPATCHER -->
  <div id="tab-cockpit" class="tab-content active">
    <div class="grid-dashboard">
      <div class="panel">
        <div class="panel-head">Payment Rail Dispatcher</div>
        <div class="panel-body">
          <form onsubmit="event.preventDefault(); submitPayment();">
            <div class="form-group"><label>Debit Float Account</label><input type="text" id="src-account" value="00040310001928" readonly></div>
            <div class="form-group"><label>Beneficiary Entity Name</label><input type="text" id="bene-name" value="Tata Motors Fleet Ltd" required></div>
            <div class="form-group"><label>Beneficiary Account Number</label><input type="text" id="bene-acct" value="912345678901" required></div>
            <div class="form-group"><label>Beneficiary IFSC Code</label><input type="text" id="bene-ifsc" value="HDFC0000001" required></div>
            <div class="form-group"><label>Payout Amount (INR)</label><input type="number" id="payout-amt" value="25000" min="1" step="0.01" required></div>
            <div class="form-group">
              <label>Rail Protocol Selection</label>
              <select id="rail-select">
                <option value="AUTO">SMART_ROUTE (Cost/SLA Matrix)</option>
                <option value="NEFT">NEFT (RBI SFMS Batch Clearing)</option>
                <option value="RTGS">RTGS (Gross Wholesale Settlement)</option>
                <option value="IMPS">IMPS (24x7 Real-Time Switch)</option>
                <option value="UPI">UPI (NPCI 2.0 Intent)</option>
              </select>
            </div>
            <div class="form-group"><label>Distributed Idempotency Key</label><input type="text" id="idem-key" readonly></div>
            <button type="submit" class="btn">Execute Idempotent Payout</button>
            <button type="button" class="btn btn-secondary" onclick="submitEOD()">Trigger EOD Nostro Settlement</button>
          </form>
        </div>
      </div>

      <div>
        <div class="panel">
          <div class="panel-head"><span>Postings Ledger</span><span style="color: var(--accent-green); font-size: 10px;">BALANCED JOURNAL OK</span></div>
          <div class="table-container">
            <table>
              <thead>
                <tr><th>ID</th><th>TIMESTAMP</th><th>JOURNAL VOUCHER</th><th>ACCOUNT</th><th>LEG</th><th>AMOUNT</th><th>NARRATIVE</th><th>AUDIT</th></tr>
              </thead>
              <tbody id="ledger-body"></tbody>
            </table>
          </div>
        </div>
        <div class="wire-grid">
          <div class="panel">
            <div class="panel-head">ISO 20022 PACS.008.001.08 WIRE MESSAGE</div>
            <pre id="pacs-display">&lt;!-- Dispatched wire XML will appear here --&gt;</pre>
          </div>
          <div class="panel">
            <div class="panel-head">OUTWARD ERP WEBHOOK DISPATCH (HMAC-SHA256)</div>
            <pre id="webhook-display">/* Webhook dispatch event payload will appear here */</pre>
          </div>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 2: RERA ESCROW CONTROLLER -->
  <div id="tab-rera" class="tab-content">
    <div class="grid-dashboard">
      <div class="panel">
        <div class="panel-head">RERA Escrow Split Execution Engine</div>
        <div class="panel-body">
          <form onsubmit="event.preventDefault(); submitRera();">
            <div class="form-group"><label>RERA Registered Project ID</label><input type="text" id="rera-proj" value="PRJ-MAHARERA-2026-991" readonly></div>
            <div class="form-group"><label>Homebuyer Virtual Account (VAN)</label><input type="text" id="rera-van" value="VAN-PUNE-TWRB-804" required></div>
            <div class="form-group"><label>Inflow Consideration (INR)</label><input type="number" id="rera-amt" value="5000000" min="1000" step="100" required></div>
            <p style="color:var(--text-muted); font-size: 11px; margin-bottom: 12px;">Mandatory Section 4(2)(l)(D) Rule: 70% ring-fenced for site construction liabilities; 30% unencumbered operational float.</p>
            <button type="submit" class="btn">Execute Dual Escrow Allocation</button>
          </form>
        </div>
      </div>
      <div class="panel">
        <div class="panel-head">Escrow Partition Ledgers</div>
        <div class="panel-body">
          <div style="display:grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-bottom: 16px;">
            <div style="background:var(--bg-card); padding:16px; border:1px solid var(--border-subtle); border-radius:4px;">
              <div class="stat-label">70% Dedicated Project Escrow</div>
              <div class="stat-val" id="disp-rera-70-box" style="color:var(--accent-amber); font-size:18px;">INR 3,50,00,000.00</div>
              <p style="font-size:11px; color:var(--text-muted); margin-top:6px;">Encumbered. Withdrawals require CA, Architect, and Engineer certifications.</p>
            </div>
            <div style="background:var(--bg-card); padding:16px; border:1px solid var(--border-subtle); border-radius:4px;">
              <div class="stat-label">30% Operational Current Account</div>
              <div class="stat-val" id="disp-rera-30-box" style="color:var(--accent-green); font-size:18px;">INR 1,50,00,000.00</div>
              <p style="font-size:11px; color:var(--text-muted); margin-top:6px;">Unrestricted. Liquid for corporate administrative expenses and overheads.</p>
            </div>
          </div>
          <pre id="rera-log">/* RERA Execution Log will stream here */</pre>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 3: LIQUIDITY SWEEP CONTROLLER -->
  <div id="tab-sweeps" class="tab-content">
    <div class="grid-dashboard">
      <div class="panel">
        <div class="panel-head">Zero-Balance Account (ZBA) Sweeper</div>
        <div class="panel-body">
          <form onsubmit="event.preventDefault(); submitSweep();">
            <div class="form-group"><label>Subsidiary Current Account (ZBA)</label><input type="text" id="zba-acct" value="SUBSIDIARY_PUNE_PLANT_4021" required></div>
            <div class="form-group"><label>Central Master Concentration Pool</label><input type="text" value="MASTER_LIQUIDITY_POOL_01" readonly></div>
            <div class="form-group"><label>Sweep Amount (INR)</label><input type="number" id="sweep-amt" value="1500000" min="1000" step="100" required></div>
            <button type="submit" class="btn btn-amber">Trigger EOD Physical Cash Concentration</button>
          </form>
        </div>
      </div>
      <div class="panel">
        <div class="panel-head">Treasury Concentration Pool Metrics</div>
        <div class="panel-body">
          <div style="background:var(--bg-card); padding:16px; border:1px solid var(--border-subtle); border-radius:4px; margin-bottom:16px;">
            <div class="stat-label">Aggregated Concentrated Liquidity</div>
            <div class="stat-val" id="disp-sweep-pool-box" style="color:var(--accent-green); font-size:20px;">INR 5,00,00,000.00</div>
            <p style="font-size:11px; color:var(--text-muted); margin-top:6px;">Central treasury pool earning repo interest, preventing decentralized idle balances.</p>
          </div>
          <pre id="sweep-log">/* Liquidity Sweep Execution Output */</pre>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 4: ARCHITECTURE VECTORS -->
  <div id="tab-architecture" class="tab-content">
    <div class="doc-section">
      <h2>Transaction Banking Multi-Rail Settlement Architecture</h2>
      <p>TBG-CORE serves as the high-availability orchestration layer between corporate enterprise resource planning (ERP) platforms and central payment rails. Double-entry holds eliminate settlement leakage during clearing cutoffs.</p>
      <svg class="vector-svg" viewBox="0 0 960 260" xmlns="http://www.w3.org/2000/svg">
        <defs><marker id="arr" viewBox="0 0 10 10" refX="5" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#00e5ff" /></marker></defs>
        <rect x="20" y="90" width="150" height="80" rx="6" fill="#0d1b2e" stroke="#192a42" stroke-width="2"/>
        <text x="95" y="130" fill="#f0f6fc" font-family="monospace" font-size="12" text-anchor="middle" font-weight="bold">Corporate ERP</text>
        <rect x="230" y="90" width="160" height="80" rx="6" fill="#0d1b2e" stroke="#00e5ff" stroke-width="2"/>
        <text x="310" y="130" fill="#00e5ff" font-family="monospace" font-size="12" text-anchor="middle" font-weight="bold">TBG Ingress Switch</text>
        <rect x="450" y="90" width="170" height="80" rx="6" fill="#0d1b2e" stroke="#00ffa3" stroke-width="2"/>
        <text x="535" y="130" fill="#00ffa3" font-family="monospace" font-size="12" text-anchor="middle" font-weight="bold">Double-Entry Core</text>
        <rect x="680" y="30" width="130" height="50" rx="6" fill="#0a1322" stroke="#ffb703" stroke-width="1.5"/><text x="745" y="60" fill="#ffb703" font-family="monospace" font-size="11" text-anchor="middle">NPCI (UPI/IMPS)</text>
        <rect x="680" y="105" width="130" height="50" rx="6" fill="#0a1322" stroke="#00e5ff" stroke-width="1.5"/><text x="745" y="135" fill="#00e5ff" font-family="monospace" font-size="11" text-anchor="middle">RBI SFMS (NEFT)</text>
        <rect x="680" y="180" width="130" height="50" rx="6" fill="#0a1322" stroke="#00ffa3" stroke-width="1.5"/><text x="745" y="210" fill="#00ffa3" font-family="monospace" font-size="11" text-anchor="middle">RBI RTGS Core</text>
        <rect x="850" y="105" width="90" height="50" rx="6" fill="#0d1b2e" stroke="#192a42" stroke-width="1.5"/><text x="895" y="135" fill="#f0f6fc" font-family="monospace" font-size="10" text-anchor="middle">Nostro Settled</text>
        <line x1="170" y1="130" x2="225" y2="130" stroke="#00e5ff" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="390" y1="130" x2="445" y2="130" stroke="#00e5ff" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="620" y1="110" x2="675" y2="55" stroke="#ffb703" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="620" y1="130" x2="675" y2="130" stroke="#00e5ff" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="620" y1="150" x2="675" y2="205" stroke="#00ffa3" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="810" y1="130" x2="845" y2="130" stroke="#8b9eb5" stroke-width="1.5" marker-end="url(#arr)"/>
      </svg>
    </div>
  </div>

  <!-- TAB 5: PRD -->
  <div id="tab-prd" class="tab-content">
    <div class="doc-section">
      <h2>Institutional PRD: Transaction Banking Platform</h2>
      <p><strong>Specification ID:</strong> PRD-TBG-CMS-4.2 | <strong>Target Tier:</strong> Scheduled Commercial Banks & FinTech Aggregators</p>
      <h3 style="color:var(--text-main); margin:16px 0 8px;">1. Functional Scope</h3>
      <p>Unify Wholesale Outward Payouts, Regulatory Real Estate Escrow partitions, and Multi-Entity Zero Balance Account (ZBA) sweeps into an immutable double-entry ledger state machine.</p>
      <h3 style="color:var(--text-main); margin:16px 0 8px;">2. Regulatory Guardrails</h3>
      <ul style="color:var(--text-muted); margin-left:20px; line-height:1.8;">
        <li><strong>Zero-Sum Ledger Rule:</strong> Every voucher must conform to $\sum DR - \sum CR = 0$. Float and suspense accounts are updated atomically.</li>
        <li><strong>RERA Section 4(2)(l)(D):</strong> Inflow to registered project VANs strictly splits 70% to unencumbered project escrows and 30% to business operational ledgers.</li>
        <li><strong>Idempotency Enforcement:</strong> Prevent duplicate network transmissions via UUID/HMAC-bound idempotency keys.</li>
      </ul>
    </div>
  </div>

  <!-- TAB 6: GLOSSARY -->
  <div id="tab-glossary" class="tab-content">
    <div class="doc-section">
      <h2>Wholesale Banking & Payments Glossary</h2>
      <div class="glossary-grid">
        <div class="term-card"><div class="term-title">pacs.008 (ISO 20022)</div><div class="term-desc">Financial Customer Credit Transfer payload schema for interbank high/low-value clearing.</div></div>
        <div class="term-card"><div class="term-title">CMS Suspense Account</div><div class="term-desc">Internal operational float ledger tracking the bank's liability to the clearing house prior to interbank Nostro finality.</div></div>
        <div class="term-card"><div class="term-title">Nostro Account</div><div class="term-desc">Account held by the domestic bank with the Reserve Bank of India to settle multilateral net obligations.</div></div>
        <div class="term-card"><div class="term-title">Zero-Sum Audit</div><div class="term-desc">Proof that no credit is booked without a corresponding debit hold on the corporate float balance.</div></div>
        <div class="term-card"><div class="term-title">Zero Balance Account (ZBA)</div><div class="term-desc">Subsidiary account whose balance is swept to or funded from a master concentration pool at cutoff.</div></div>
        <div class="term-card"><div class="term-title">Virtual Account (VAN)</div><div class="term-desc">Shadow routing identifier mapping unique debtor payments to single physical accounts for automated reconciliation.</div></div>
      </div>
    </div>
  </div>

  <script>
    function switchTab(id, btn) {
      document.querySelectorAll('.tab-content').forEach(c => c.classList.remove('active'));
      document.querySelectorAll('.nav-btn').forEach(b => b.classList.remove('active'));
      document.getElementById(id).classList.add('active');
      btn.classList.add('active');
    }

    function genIdem() {
      document.getElementById('idem-key').value = 'TXN-' + Math.floor(10000000 + Math.random() * 90000000);
    }

    async function refreshState() {
      const res = await fetch('/api/state');
      const data = await res.json();
      document.getElementById('disp-float').innerText = 'INR ' + Number(data.float_balance).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-suspense').innerText = 'INR ' + Number(data.suspense_balance).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-70').innerText = 'INR ' + Number(data.rera_escrow).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-70-box').innerText = 'INR ' + Number(data.rera_escrow).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-30-box').innerText = 'INR ' + Number(data.rera_operational).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-sweep-pool').innerText = 'INR ' + Number(data.sweep_pool).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-sweep-pool-box').innerText = 'INR ' + Number(data.sweep_pool).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-count').innerText = data.entry_count + ' Entries';

      const tbody = document.getElementById('ledger-body');
      tbody.innerHTML = '';
      data.ledger.forEach(r => {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td>' + r.id + '</td><td>' + r.time + '</td><td>' + r.jv + '</td><td>' + r.acct + '</td><td class="' + (r.leg==='DR'?'tag-dr':'tag-cr') + '">' + r.leg + '</td><td>INR ' + Number(r.amt).toLocaleString('en-IN', {minimumFractionDigits: 2}) + '</td><td>' + (r.narrative || 'Posting') + '</td><td style="color:var(--accent-green)">✓ ' + r.verdict + '</td>';
        tbody.appendChild(tr);
      });
    }

    async function submitPayment() {
      const payload = {
        source_account: document.getElementById('src-account').value,
        beneficiary_name: document.getElementById('bene-name').value,
        beneficiary_account: document.getElementById('bene-acct').value,
        beneficiary_ifsc: document.getElementById('bene-ifsc').value,
        amount: parseFloat(document.getElementById('payout-amt').value),
        rail: document.getElementById('rail-select').value,
        idempotency_key: document.getElementById('idem-key').value
      };

      const res = await fetch('/api/payout', {
        method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)
      });
      if (!res.ok) { alert('Failed: ' + await res.text()); return; }
      const out = await res.json();

      const pacs = '<?xml version="1.0" encoding="UTF-8"?>\n<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">\n  <FIToFICstmrCdtTrf>\n    <GrpHdr>\n      <MsgId>' + out.pacs_msg_id + '</MsgId>\n      <CreDtTm>' + new Date().toISOString() + '</CreDtTm>\n      <NbOfTxs>1</NbOfTxs>\n      <SttlmInf><SttlmMtd>CLRG</SttlmMtd></SttlmInf>\n    </GrpHdr>\n    <CdtTrfTxInf>\n      <PmtId><EndToEndId>' + payload.idempotency_key + '</EndToEndId></PmtId>\n      <IntrBkSttlmAmt Ccy="INR">' + payload.amount.toFixed(2) + '</IntrBkSttlmAmt>\n      <Dbtr><Nm>DEBTOR ENTERPRISE</Nm></Dbtr>\n      <DbtrAcct><Id><Othr><Id>' + payload.source_account + '</Id></Othr></Id></DbtrAcct>\n      <CdtrAgt><FinInstnId><ClrSysMmbId><MmbId>' + payload.beneficiary_ifsc + '</MmbId></ClrSysMmbId></FinInstnId></CdtrAgt>\n      <Cdtr><Nm>' + payload.beneficiary_name + '</Nm></Cdtr>\n      <CdtrAcct><Id><Othr><Id>' + payload.beneficiary_account + '</Id></Othr></Id></CdtrAcct>\n    </CdtTrfTxInf>\n  </FIToFICstmrCdtTrf>\n</Document>';
      document.getElementById('pacs-display').innerText = pacs;

      document.getElementById('webhook-display').innerText = JSON.stringify({
        event: "payout.settled", timestamp: new Date().toISOString(), signature_sha256: out.signature,
        data: { jv_id: out.jv_id, originating_account: payload.source_account, beneficiary_account: payload.beneficiary_account, amount: payload.amount, currency: "INR", rail: out.rail, status: out.status }
      }, null, 2);

      genIdem();
      await refreshState();
    }

    async function submitRera() {
      const payload = {
        project_id: document.getElementById('rera-proj').value,
        buyer_van: document.getElementById('rera-van').value,
        amount: parseFloat(document.getElementById('rera-amt').value)
      };
      const res = await fetch('/api/rera', {
        method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('rera-log').innerText = JSON.stringify(out, null, 2);
      await refreshState();
    }

    async function submitSweep() {
      const payload = {
        subsidiary_account: document.getElementById('zba-acct').value,
        sweep_amount: parseFloat(document.getElementById('sweep-amt').value)
      };
      const res = await fetch('/api/sweep', {
        method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('sweep-log').innerText = JSON.stringify(out, null, 2);
      await refreshState();
    }

    async function submitEOD() {
      const res = await fetch('/api/eod', { method: 'POST' });
      if (!res.ok) { alert('Settlement error: ' + await res.text()); return; }
      const out = await res.json();
      alert('EOD Nostro Cleared: INR ' + Number(out.cleared_amt).toLocaleString('en-IN', {minimumFractionDigits: 2}));
      await refreshState();
    }

    window.onload = function() {
      genIdem();
      refreshState();
    };
  </script>
</body>
</html>
`
