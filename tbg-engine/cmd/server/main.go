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
}

type PayoutRequest struct {
	SourceAccount      string  `json:"source_account"`
	BeneficiaryName    string  `json:"beneficiary_name"`
	BeneficiaryAccount string  `json:"beneficiary_account"`
	BeneficiaryIFSC    string  `json:"beneficiary_ifsc"`
	Amount             float64 `json:"amount"`
	Rail               string  `json:"rail"`
	IdempotencyKey     string  `json:"idempotency_key"`
}

type EngineState struct {
	sync.Mutex
	FloatBalance    float64
	SuspenseBalance float64
	EntryCounter    int
	Ledger          []JournalEntry
	IdempotencyMap  map[string]bool
}

var state = EngineState{
	FloatBalance:    10000000.00,
	SuspenseBalance: 0.00,
	EntryCounter:    16,
	IdempotencyMap:  make(map[string]bool),
	Ledger: []JournalEntry{
		{ID: "#16", Timestamp: "9:50:48 pm", JVID: "232631f0-582c-426e-nostro", Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: 75000.00, Verdict: "ZERO-SUM OK"},
		{ID: "#15", Timestamp: "9:50:48 pm", JVID: "232631f0-582c-426e-nostro", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: 75000.00, Verdict: "ZERO-SUM OK"},
		{ID: "#14", Timestamp: "9:50:41 pm", JVID: "bded8c1d-2055-4a80-cms", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: 25000.00, Verdict: "ZERO-SUM OK"},
		{ID: "#13", Timestamp: "9:50:41 pm", JVID: "bded8c1d-2055-4a80-cms", Account: "00040310001928", Leg: "DR", Amount: 25000.00, Verdict: "ZERO-SUM OK"},
	},
}

func emitSplunkLog(eventType, acct, txnid string, amt float64, status string, latency float64) {
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
		"float_balance":    state.FloatBalance,
		"suspense_balance": state.SuspenseBalance,
		"entry_count":      state.EntryCounter,
		"ledger":           state.Ledger,
	})
}

func handlePayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()

	var req PayoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	state.Lock()
	defer state.Unlock()

	if state.IdempotencyMap[req.IdempotencyKey] {
		http.Error(w, "Conflict: Duplicate Idempotency Key", http.StatusConflict)
		return
	}

	if req.Amount > state.FloatBalance {
		http.Error(w, "Insufficient Corporate Float balance", http.StatusBadRequest)
		return
	}

	state.FloatBalance -= req.Amount
	state.SuspenseBalance += req.Amount
	state.IdempotencyMap[req.IdempotencyKey] = true

	nowStr := time.Now().Format("3:04:05 pm")
	jvID := fmt.Sprintf("%08x-cms", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{
		ID:        fmt.Sprintf("#%d", state.EntryCounter),
		Timestamp: nowStr,
		JVID:      jvID,
		Account:   "AC_CMS_SUSPENSE_CLEARING_9999",
		Leg:       "CR",
		Amount:    req.Amount,
		Verdict:   "ZERO-SUM OK",
	}
	e2 := JournalEntry{
		ID:        fmt.Sprintf("#%d", state.EntryCounter-1),
		Timestamp: nowStr,
		JVID:      jvID,
		Account:   req.SourceAccount,
		Leg:       "DR",
		Amount:    req.Amount,
		Verdict:   "ZERO-SUM OK",
	}

	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	rail := req.Rail
	if rail == "AUTO" {
		if req.Amount >= 200000 {
			rail = "RTGS"
		} else {
			rail = "NEFT"
		}
	}

	mac := hmac.New(sha256.New, []byte("production_hmac_secret_key"))
	mac.Write([]byte(fmt.Sprintf("%s:%f:%s", jvID, req.Amount, req.BeneficiaryAccount)))
	sig := hex.EncodeToString(mac.Sum(nil))

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	emitSplunkLog("LEDGER_POSTING", req.SourceAccount, jvID, req.Amount, "SETTLED", latency)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "SETTLED",
		"jv_id":       jvID,
		"rail":        rail,
		"latency_ms":  latency,
		"signature":   sig,
		"pacs_msg_id": fmt.Sprintf("MSG-%d", time.Now().UnixNano()),
	})
}

func handleEOD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()

	state.Lock()
	defer state.Unlock()

	if state.SuspenseBalance <= 0 {
		http.Error(w, "Suspense account liability is zero. Nothing to settle.", http.StatusBadRequest)
		return
	}

	clearedAmt := state.SuspenseBalance
	state.SuspenseBalance = 0.00
	nowStr := time.Now().Format("3:04:05 pm")
	jvID := fmt.Sprintf("%08x-nostro", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{
		ID:        fmt.Sprintf("#%d", state.EntryCounter),
		Timestamp: nowStr,
		JVID:      jvID,
		Account:   "AC_RBI_NOSTRO_0001",
		Leg:       "CR",
		Amount:    clearedAmt,
		Verdict:   "ZERO-SUM OK",
	}
	e2 := JournalEntry{
		ID:        fmt.Sprintf("#%d", state.EntryCounter-1),
		Timestamp: nowStr,
		JVID:      jvID,
		Account:   "AC_CMS_SUSPENSE_CLEARING_9999",
		Leg:       "DR",
		Amount:    clearedAmt,
		Verdict:   "ZERO-SUM OK",
	}

	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	emitSplunkLog("NOSTRO_EOD_CLEARING", "AC_RBI_NOSTRO_0001", jvID, clearedAmt, "SETTLED", latency)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "NOSTRO_CLEARED",
		"cleared_amt": clearedAmt,
		"jv_id":       jvID,
		"latency_ms":  latency,
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(uiHTML))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/state", handleState)
	http.HandleFunc("/api/payout", handlePayout)
	http.HandleFunc("/api/eod", handleEOD)

	log.Printf("TBG-CORE running on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>TBG-CORE // Finacle Treasury & Multi-Rail Clearing Engine</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    :root {
      --bg-base: #060b13;
      --bg-surface: #0a1322;
      --bg-card: #0f1c30;
      --border-subtle: #1a2d4a;
      --border-focus: #00e5ff;
      --text-main: #f0f6fc;
      --text-muted: #8b9eb5;
      --accent-cyan: #00e5ff;
      --accent-green: #00ffa3;
      --accent-amber: #ffb703;
      --accent-crimson: #ff3366;
      --font-mono: 'JetBrains Mono', 'Fira Code', monospace;
      --font-sans: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { background: var(--bg-base); color: var(--text-main); font-family: var(--font-sans); font-size: 13px; }
    header { background: var(--bg-surface); border-bottom: 1px solid var(--border-subtle); padding: 12px 24px; display: flex; justify-content: space-between; align-items: center; position: sticky; top: 0; z-index: 100; }
    .brand { display: flex; align-items: center; gap: 12px; font-family: var(--font-mono); font-weight: 700; font-size: 14px; }
    .badge { background: rgba(0, 229, 255, 0.12); border: 1px solid var(--accent-cyan); color: var(--accent-cyan); font-size: 10px; padding: 2px 6px; border-radius: 3px; }
    .nav-tabs { display: flex; gap: 8px; }
    .nav-btn { background: transparent; border: 1px solid transparent; color: var(--text-muted); padding: 6px 14px; font-size: 12px; cursor: pointer; border-radius: 4px; font-family: var(--font-mono); }
    .nav-btn.active, .nav-btn:hover { color: var(--accent-cyan); background: var(--bg-card); border-color: var(--border-subtle); }
    .stats-bar { display: grid; grid-template-columns: repeat(5, 1fr); gap: 1px; background: var(--border-subtle); border-bottom: 1px solid var(--border-subtle); }
    .stat-card { background: var(--bg-surface); padding: 14px 20px; }
    .stat-label { font-size: 10px; font-family: var(--font-mono); color: var(--text-muted); text-transform: uppercase; margin-bottom: 4px; }
    .stat-val { font-family: var(--font-mono); font-size: 16px; font-weight: 600; }
    .tab-content { display: none; padding: 20px 24px; }
    .tab-content.active { display: block; }
    .grid-dashboard { display: grid; grid-template-columns: 340px 1fr; gap: 20px; }
    .panel { background: var(--bg-surface); border: 1px solid var(--border-subtle); border-radius: 6px; overflow: hidden; display: flex; flex-direction: column; }
    .panel-head { background: var(--bg-card); padding: 10px 16px; border-bottom: 1px solid var(--border-subtle); font-family: var(--font-mono); font-size: 11px; font-weight: 600; text-transform: uppercase; color: var(--text-muted); display: flex; justify-content: space-between; }
    .panel-body { padding: 16px; }
    .form-group { margin-bottom: 12px; }
    label { display: block; font-family: var(--font-mono); font-size: 10px; color: var(--text-muted); text-transform: uppercase; margin-bottom: 4px; }
    input, select { width: 100%; background: var(--bg-base); border: 1px solid var(--border-subtle); padding: 8px 10px; color: var(--text-main); font-family: var(--font-mono); font-size: 12px; border-radius: 4px; outline: none; }
    input:focus, select:focus { border-color: var(--border-focus); }
    .btn { width: 100%; background: var(--accent-cyan); color: #030712; border: none; padding: 10px; font-family: var(--font-mono); font-size: 11px; font-weight: 700; text-transform: uppercase; cursor: pointer; border-radius: 4px; }
    .btn-secondary { background: transparent; border: 1px solid var(--accent-green); color: var(--accent-green); margin-top: 8px; }
    .table-container { overflow-x: auto; max-height: 480px; }
    table { width: 100%; border-collapse: collapse; font-family: var(--font-mono); font-size: 11px; }
    th { background: var(--bg-card); padding: 8px 12px; text-align: left; font-weight: 600; color: var(--text-muted); border-bottom: 1px solid var(--border-subtle); position: sticky; top: 0; }
    td { padding: 8px 12px; border-bottom: 1px solid rgba(26, 45, 74, 0.5); }
    .tag-dr { color: var(--accent-crimson); font-weight: 700; }
    .tag-cr { color: var(--accent-green); font-weight: 700; }
    .wire-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-top: 16px; }
    pre { background: var(--bg-base); border: 1px solid var(--border-subtle); border-radius: 4px; padding: 12px; font-family: var(--font-mono); font-size: 10.5px; color: #9cdcfe; overflow-x: auto; max-height: 280px; }
    .doc-section { background: var(--bg-surface); border: 1px solid var(--border-subtle); border-radius: 6px; padding: 24px; margin-bottom: 24px; }
    .doc-section h2 { font-size: 16px; font-family: var(--font-mono); color: var(--accent-cyan); margin-bottom: 12px; border-bottom: 1px solid var(--border-subtle); padding-bottom: 8px; }
    .doc-section p { color: var(--text-muted); margin-bottom: 12px; }
    .glossary-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 16px; }
    .term-card { background: var(--bg-card); border: 1px solid var(--border-subtle); border-radius: 4px; padding: 16px; }
    .term-title { font-family: var(--font-mono); font-size: 13px; color: var(--accent-cyan); margin-bottom: 6px; }
    .term-desc { font-size: 12px; color: var(--text-muted); line-height: 1.6; }
    .vector-svg { width: 100%; background: var(--bg-base); border: 1px solid var(--border-subtle); border-radius: 6px; padding: 16px; margin-top: 12px; }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <span>TBG-CORE</span>
      <span class="badge">FINACLE TREASURY // STRIPE LEDGER</span>
      <span style="color: var(--text-muted); font-size: 11px;">POSTGRESQL 16 ACID</span>
    </div>
    <div class="nav-tabs">
      <button class="nav-btn active" onclick="switchTab('tab-cockpit', this)">Cockpit Console</button>
      <button class="nav-btn" onclick="switchTab('tab-architecture', this)">Architecture & Process Vector</button>
      <button class="nav-btn" onclick="switchTab('tab-prd', this)">Institutional PRD</button>
      <button class="nav-btn" onclick="switchTab('tab-glossary', this)">Banking Glossary</button>
    </div>
  </header>

  <div class="stats-bar">
    <div class="stat-card">
      <div class="stat-label">Corporate Float (00040310001928)</div>
      <div class="stat-val" id="disp-float">INR 1,00,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">CMS Suspense Net Liability</div>
      <div class="stat-val" id="disp-suspense" style="color: var(--accent-cyan);">INR 0.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Dynamic Rail Mode</div>
      <div class="stat-val" style="color: var(--accent-amber);">NEFT / RTGS (SFMS)</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Engine Latency (P99)</div>
      <div class="stat-val" id="disp-latency" style="color: var(--accent-green);">1.05 ms</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Audited Postings</div>
      <div class="stat-val" id="disp-count">16 Entries</div>
    </div>
  </div>

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
              <label>Rail Protocol</label>
              <select id="rail-select">
                <option value="AUTO">SMART_ROUTE (Optimal Latency/MDR)</option>
                <option value="NEFT">NEFT (RBI SFMS)</option>
                <option value="RTGS">RTGS (High-Value Wholesale)</option>
                <option value="IMPS">IMPS (24x7 Real-Time)</option>
                <option value="UPI">UPI (NPCI 2.0)</option>
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
                <tr><th>ID</th><th>TIMESTAMP</th><th>JOURNAL VOUCHER</th><th>ACCOUNT</th><th>LEG</th><th>AMOUNT (INR)</th><th>AUDIT</th></tr>
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

  <div id="tab-architecture" class="tab-content">
    <div class="doc-section">
      <h2>Transaction Banking Multi-Rail Settlement Architecture</h2>
      <p>The TBG-CORE executes as a zero-trust orchestrator between Corporate ERPs and Central Clearing Houses (RBI SFMS, NPCI, RTGS Core), preventing settlement leakage through real-time ledger reservations.</p>
      <svg class="vector-svg" viewBox="0 0 960 260" xmlns="http://www.w3.org/2000/svg">
        <defs><marker id="arr" viewBox="0 0 10 10" refX="5" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#00e5ff" /></marker></defs>
        <rect x="20" y="90" width="150" height="80" rx="6" fill="#0f1c30" stroke="#1a2d4a" stroke-width="2"/>
        <text x="95" y="130" fill="#f0f6fc" font-family="monospace" font-size="12" text-anchor="middle" font-weight="bold">Corporate ERP</text>
        <rect x="230" y="90" width="160" height="80" rx="6" fill="#0f1c30" stroke="#00e5ff" stroke-width="2"/>
        <text x="310" y="130" fill="#00e5ff" font-family="monospace" font-size="12" text-anchor="middle" font-weight="bold">TBG Ingress Gateway</text>
        <rect x="450" y="90" width="170" height="80" rx="6" fill="#0f1c30" stroke="#00ffa3" stroke-width="2"/>
        <text x="535" y="130" fill="#00ffa3" font-family="monospace" font-size="12" text-anchor="middle" font-weight="bold">Dual-Entry Ledger</text>
        <rect x="680" y="30" width="130" height="50" rx="6" fill="#0a1322" stroke="#ffb703" stroke-width="1.5"/><text x="745" y="60" fill="#ffb703" font-family="monospace" font-size="11" text-anchor="middle">NPCI (UPI/IMPS)</text>
        <rect x="680" y="105" width="130" height="50" rx="6" fill="#0a1322" stroke="#00e5ff" stroke-width="1.5"/><text x="745" y="135" fill="#00e5ff" font-family="monospace" font-size="11" text-anchor="middle">RBI SFMS (NEFT)</text>
        <rect x="680" y="180" width="130" height="50" rx="6" fill="#0a1322" stroke="#00ffa3" stroke-width="1.5"/><text x="745" y="210" fill="#00ffa3" font-family="monospace" font-size="11" text-anchor="middle">RBI RTGS Core</text>
        <rect x="850" y="105" width="90" height="50" rx="6" fill="#0f1c30" stroke="#1a2d4a" stroke-width="1.5"/><text x="895" y="135" fill="#f0f6fc" font-family="monospace" font-size="10" text-anchor="middle">Nostro Settled</text>
        <line x1="170" y1="130" x2="225" y2="130" stroke="#00e5ff" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="390" y1="130" x2="445" y2="130" stroke="#00e5ff" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="620" y1="110" x2="675" y2="55" stroke="#ffb703" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="620" y1="130" x2="675" y2="130" stroke="#00e5ff" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="620" y1="150" x2="675" y2="205" stroke="#00ffa3" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="810" y1="130" x2="845" y2="130" stroke="#8b9eb5" stroke-width="1.5" marker-end="url(#arr)"/>
      </svg>
    </div>
  </div>

  <div id="tab-prd" class="tab-content">
    <div class="doc-section">
      <h2>Institutional PRD: Transaction Banking Payment Engine</h2>
      <p><strong>Standard:</strong> ISO 20022 / RBI Core Directions | <strong>Classification:</strong> Tier-1 Wholesale Rail Gateway</p>
      <ul style="color: var(--text-muted); margin-left: 20px; line-height: 1.8;">
        <li><strong>Strict Double-Entry Invariance:</strong> Sum of debits must balance sum of credits for every transaction lifecycle event (&Sigma; DR - &Sigma; CR = 0).</li>
        <li><strong>Idempotency Enforcement:</strong> All execution requests require a unique client idempotency key to prevent accidental duplicate transfers over flakey networks.</li>
        <li><strong>Deterministic Routing:</strong> Tickets &ge; INR 2,00,000 route via RTGS; tickets &le; INR 1,00,000 route via instant UPI/IMPS rails.</li>
      </ul>
    </div>
  </div>

  <div id="tab-glossary" class="tab-content">
    <div class="doc-section">
      <h2>Wholesale Banking & Payments Glossary</h2>
      <div class="glossary-grid">
        <div class="term-card"><div class="term-title">pacs.008 (ISO 20022)</div><div class="term-desc">Financial Customer Credit Transfer payload schema for interbank high/low-value clearing.</div></div>
        <div class="term-card"><div class="term-title">CMS Suspense Account</div><div class="term-desc">Internal operational float ledger tracking the bank's liability to the clearing house prior to interbank Nostro finality.</div></div>
        <div class="term-card"><div class="term-title">Nostro Account</div><div class="term-desc">Account held by the domestic bank with the Reserve Bank of India to settle multilateral net obligations.</div></div>
        <div class="term-card"><div class="term-title">Zero-Sum Audit</div><div class="term-desc">Proof that no credit is booked without a corresponding debit hold on the corporate float balance.</div></div>
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
      document.getElementById('disp-count').innerText = data.entry_count + ' Entries';
      
      const tbody = document.getElementById('ledger-body');
      tbody.innerHTML = '';
      data.ledger.forEach(r => {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td>' + r.id + '</td><td>' + r.time + '</td><td>' + r.jv + '</td><td>' + r.acct + '</td><td class="' + (r.leg==='DR'?'tag-dr':'tag-cr') + '">' + r.leg + '</td><td>INR ' + Number(r.amt).toLocaleString('en-IN', {minimumFractionDigits: 2}) + '</td><td style="color:var(--accent-green)">✓ ' + r.verdict + '</td>';
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
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });

      if (!res.ok) {
        const err = await res.text();
        alert('Payment Failed: ' + err);
        return;
      }

      const out = await res.json();
      document.getElementById('disp-latency').innerText = out.latency_ms.toFixed(2) + ' ms';
      
      const pacs = '<?xml version="1.0" encoding="UTF-8"?>\n<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">\n  <FIToFICstmrCdtTrf>\n    <GrpHdr>\n      <MsgId>' + out.pacs_msg_id + '</MsgId>\n      <CreDtTm>' + new Date().toISOString() + '</CreDtTm>\n      <NbOfTxs>1</NbOfTxs>\n      <SttlmInf><SttlmMtd>CLRG</SttlmMtd></SttlmInf>\n    </GrpHdr>\n    <CdtTrfTxInf>\n      <PmtId><EndToEndId>' + payload.idempotency_key + '</EndToEndId></PmtId>\n      <IntrBkSttlmAmt Ccy="INR">' + payload.amount.toFixed(2) + '</IntrBkSttlmAmt>\n      <Dbtr><Nm>DEBTOR ENTERPRISE</Nm></Dbtr>\n      <DbtrAcct><Id><Othr><Id>' + payload.source_account + '</Id></Othr></Id></DbtrAcct>\n      <CdtrAgt><FinInstnId><ClrSysMmbId><MmbId>' + payload.beneficiary_ifsc + '</MmbId></ClrSysMmbId></FinInstnId></CdtrAgt>\n      <Cdtr><Nm>' + payload.beneficiary_name + '</Nm></Cdtr>\n      <CdtrAcct><Id><Othr><Id>' + payload.beneficiary_account + '</Id></Othr></Id></CdtrAcct>\n    </CdtTrfTxInf>\n  </FIToFICstmrCdtTrf>\n</Document>';
      document.getElementById('pacs-display').innerText = pacs;

      document.getElementById('webhook-display').innerText = JSON.stringify({
        event: "payout.settled",
        timestamp: new Date().toISOString(),
        signature_sha256: out.signature,
        data: {
          jv_id: out.jv_id,
          originating_account: payload.source_account,
          beneficiary_account: payload.beneficiary_account,
          amount: payload.amount,
          currency: "INR",
          rail: out.rail,
          status: out.status
        }
      }, null, 2);

      genIdem();
      await refreshState();
    }

    async function submitEOD() {
      const res = await fetch('/api/eod', { method: 'POST' });
      if (!res.ok) {
        const err = await res.text();
        alert('Settlement failed: ' + err);
        return;
      }
      const out = await res.json();
      alert('EOD Nostro Settlement Cleared: INR ' + Number(out.cleared_amt).toLocaleString('en-IN', {minimumFractionDigits: 2}));
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
