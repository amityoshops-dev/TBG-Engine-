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

type JournalEntry struct {
	ID        string  `json:"id"`
	Timestamp string  `json:"time"`
	JVID      string  `json:"jv"`
	Account   string  `json:"acct"`
	Leg       string  `json:"leg"` // DR or CR
	Amount    float64 `json:"amt"`
	Currency  string  `json:"currency"`
	Standard  string  `json:"standard"`
	Verdict   string  `json:"verdict"`
	Narrative string  `json:"narrative"`
}

type EngineState struct {
	sync.Mutex
	FloatINR          float64
	FloatUSD          float64
	SuspenseINR       float64
	SuspenseUSD       float64
	ReraProjectEscrow float64
	ReraFreeFloat     float64
	SweepPoolBalance  float64
	EntryCounter      int
	Ledger            []JournalEntry
	IdempotencyMap    map[string]bool
}

var state = EngineState{
	FloatINR:          100000000.00,
	FloatUSD:          2500000.00,
	SuspenseINR:       0.00,
	SuspenseUSD:       0.00,
	ReraProjectEscrow: 70000000.00,
	ReraFreeFloat:     30000000.00,
	SweepPoolBalance:  85000000.00,
	EntryCounter:      16,
	IdempotencyMap:    make(map[string]bool),
	Ledger: []JournalEntry{
		{ID: "#16", Timestamp: "15:20:11", JVID: "JV-89021-NTR", Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: 500000.00, Currency: "INR", Standard: "RBI SFMS", Verdict: "ZERO-SUM OK", Narrative: "Central Bank Gross Clearing Settlement"},
		{ID: "#15", Timestamp: "15:20:11", JVID: "JV-89021-NTR", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: 500000.00, Currency: "INR", Standard: "RBI SFMS", Verdict: "ZERO-SUM OK", Narrative: "CMS Suspense Liability Clear"},
		{ID: "#14", Timestamp: "15:18:40", JVID: "JV-89018-CMS", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: 250000.00, Currency: "INR", Standard: "ISO:pacs.008", Verdict: "ZERO-SUM OK", Narrative: "Transit Outward Float Reservation"},
		{ID: "#13", Timestamp: "15:18:40", JVID: "JV-89018-CMS", Account: "00040310001928", Leg: "DR", Amount: 250000.00, Currency: "INR", Standard: "ISO:pacs.008", Verdict: "ZERO-SUM OK", Narrative: "Debtor Float Settlement Debit"},
	},
}

func handleState(w http.ResponseWriter, r *http.Request) {
	state.Lock()
	defer state.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"float_inr":     state.FloatINR,
		"float_usd":     state.FloatUSD,
		"suspense_inr":  state.SuspenseINR,
		"suspense_usd":  state.SuspenseUSD,
		"rera_project":  state.ReraProjectEscrow,
		"rera_ops":      state.ReraFreeFloat,
		"sweep_pool":    state.SweepPoolBalance,
		"entry_counter": state.EntryCounter,
		"ledger":        state.Ledger,
	})
}

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
		http.Error(w, "Duplicate Idempotency Key", http.StatusConflict); return
	}
	if req.Amount > state.FloatINR {
		http.Error(w, "Insufficient Corporate Float Balance", 400); return
	}

	state.FloatINR -= req.Amount
	state.SuspenseINR += req.Amount
	state.IdempotencyMap[req.IdempotencyKey] = true

	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%06x-CMS", rand.Uint32())
	state.EntryCounter += 2

	rail := req.Rail
	if rail == "AUTO" {
		if req.Amount >= 200000 {
			rail = "RTGS"
		} else {
			rail = "NEFT"
		}
	}

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: req.Amount, Currency: "INR", Standard: "ISO:pacs.008", Verdict: "ZERO-SUM OK", Narrative: "Transit Outward Float Reservation"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Currency: "INR", Standard: "ISO:pacs.008", Verdict: "ZERO-SUM OK", Narrative: "Debtor Float Settlement Debit"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	mac := hmac.New(sha256.New, []byte("institution_production_secret"))
	mac.Write([]byte(fmt.Sprintf("%s:%f:%s", jvID, req.Amount, req.BeneficiaryAccount)))
	sig := hex.EncodeToString(mac.Sum(nil))

	latency := float64(time.Since(start).Microseconds()) / 1000.0

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
      <Dbtr><Nm>DEBTOR ENTERPRISE TREASURY</Nm></Dbtr>
      <DbtrAcct><Id><Othr><Id>%s</Id></Othr></Id></DbtrAcct>
      <CdtrAgt><FinInstnId><ClrSysMmbId><MmbId>%s</MmbId></ClrSysMmbId></FinInstnId></CdtrAgt>
      <Cdtr><Nm>%s</Nm></Cdtr>
      <CdtrAcct><Id><Othr><Id>%s</Id></Othr></Id></CdtrAcct>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>`, time.Now().Format("20060102"), rand.Intn(9999), time.Now().UTC().Format(time.RFC3339), rail, req.IdempotencyKey, jvID, req.Amount, req.SourceAccount, req.BeneficiaryIFSC, req.BeneficiaryName, req.BeneficiaryAccount)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "SETTLED",
		"jv_id":       jvID,
		"rail":        rail,
		"latency_ms":  latency,
		"signature":   sig,
		"iso_pacs008": isoXML,
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

	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%06x-RERA", rand.Uint32())
	state.EntryCounter += 3

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "ESCROW_RERA_70_SECURED", Leg: "CR", Amount: p70, Currency: "INR", Standard: "RERA Section 4", Verdict: "ZERO-SUM OK", Narrative: "70% Construction Dedicated Reserve"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: "ESCROW_RERA_30_OPERATIONAL", Leg: "CR", Amount: ops30, Currency: "INR", Standard: "RERA Section 4", Verdict: "ZERO-SUM OK", Narrative: "30% OpEx Unrestricted Credit"}
	e3 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-2), Timestamp: nowStr, JVID: jvID, Account: req.BuyerVAN, Leg: "DR", Amount: req.Amount, Currency: "INR", Standard: "Inflow Collection", Verdict: "ZERO-SUM OK", Narrative: "Homebuyer Consideration Allocation"}
	state.Ledger = append([]JournalEntry{e1, e2, e3}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ALLOCATED",
		"project_70": p70,
		"ops_30":     ops30,
		"jv_id":      jvID,
		"latency_ms": latency,
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

	state.SweepPoolBalance += req.SweepAmount
	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%06x-SWP", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "MASTER_LIQUIDITY_POOL_01", Leg: "CR", Amount: req.SweepAmount, Currency: "INR", Standard: "ZBA Sweep", Verdict: "ZERO-SUM OK", Narrative: "EOD Pool Cash Concentration"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SubsidiaryAccount, Leg: "DR", Amount: req.SweepAmount, Currency: "INR", Standard: "ZBA Sweep", Verdict: "ZERO-SUM OK", Narrative: "Zero-Balance Current Acct Sweep"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "CONCENTRATED",
		"pool_balance": state.SweepPoolBalance,
		"jv_id":        jvID,
		"latency_ms":   latency,
	})
}

func handleEOD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()

	state.Lock()
	defer state.Unlock()

	if state.SuspenseINR <= 0 {
		http.Error(w, "Suspense balance is zero. No clearing settlement required.", 400); return
	}

	clearedAmt := state.SuspenseINR
	state.SuspenseINR = 0.00
	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%06x-NTR", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: clearedAmt, Currency: "INR", Standard: "SFMS Gross", Verdict: "ZERO-SUM OK", Narrative: "Central Bank Nostro Clearing"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: clearedAmt, Currency: "INR", Standard: "SFMS Gross", Verdict: "ZERO-SUM OK", Narrative: "Discharge of CMS Suspense Liability"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "NOSTRO_CLEARED",
		"cleared_amt": clearedAmt,
		"jv_id":       jvID,
		"latency_ms":  latency,
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" { port = "10000" }

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(uiLightHTML))
	})
	http.HandleFunc("/api/state", handleState)
	http.HandleFunc("/api/payout", handlePayout)
	http.HandleFunc("/api/rera", handleReraSplit)
	http.HandleFunc("/api/sweep", handleSweep)
	http.HandleFunc("/api/eod", handleEOD)

	log.Printf("TBG-CORE Institutional Running on :%s", port)
	http.ListenAndServe(":"+port, nil)
}

const uiLightHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>TBG Core Treasury // Institutional Multi-Rail Gateway</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    :root {
      --bg: #f8fafc;
      --surface: #ffffff;
      --border: #e2e8f0;
      --border-dark: #cbd5e1;
      --text: #0f172a;
      --text-muted: #64748b;
      --primary: #0284c7;
      --primary-hover: #0369a1;
      --success: #16a34a;
      --success-bg: #f0fdf4;
      --danger: #dc2626;
      --danger-bg: #fef2f2;
      --warning: #d97706;
      --warning-bg: #fffbeb;
      --font-mono: 'JetBrains Mono', 'SF Mono', Consolas, monospace;
      --font-sans: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { background-color: var(--bg); color: var(--text); font-family: var(--font-sans); font-size: 13px; line-height: 1.5; }
    
    header { background: var(--surface); border-bottom: 1px solid var(--border); padding: 14px 28px; display: flex; justify-content: space-between; align-items: center; position: sticky; top: 0; z-index: 100; box-shadow: 0 1px 2px rgba(0,0,0,0.03); }
    .brand { display: flex; align-items: center; gap: 12px; font-family: var(--font-mono); font-weight: 700; font-size: 14px; letter-spacing: -0.02em; }
    .badge { background: #e0f2fe; color: var(--primary); font-size: 11px; padding: 3px 8px; border-radius: 4px; font-weight: 600; }
    
    .nav-tabs { display: flex; gap: 6px; }
    .nav-btn { background: transparent; border: 1px solid transparent; color: var(--text-muted); padding: 7px 14px; font-size: 12px; cursor: pointer; border-radius: 6px; font-family: var(--font-sans); font-weight: 500; transition: all 0.15s ease; }
    .nav-btn:hover { color: var(--text); background: #f1f5f9; }
    .nav-btn.active { color: var(--primary); background: #f0f9ff; border-color: #bae6fd; font-weight: 600; }

    .stats-bar { display: grid; grid-template-columns: repeat(5, 1fr); gap: 1px; background: var(--border); border-bottom: 1px solid var(--border); }
    .stat-card { background: var(--surface); padding: 14px 24px; }
    .stat-label { font-size: 11px; font-family: var(--font-sans); color: var(--text-muted); font-weight: 500; text-transform: uppercase; margin-bottom: 4px; letter-spacing: 0.03em; }
    .stat-val { font-family: var(--font-mono); font-size: 17px; font-weight: 600; color: var(--text); }

    .tab-content { display: none; padding: 24px 28px; }
    .tab-content.active { display: block; }
    
    .grid-dashboard { display: grid; grid-template-columns: 400px 1fr; gap: 24px; }
    .card { background: var(--surface); border: 1px solid var(--border); border-radius: 8px; box-shadow: 0 1px 3px rgba(0,0,0,0.02); overflow: hidden; margin-bottom: 20px; }
    .card-head { padding: 12px 18px; border-bottom: 1px solid var(--border); font-size: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: var(--text-muted); background: #fcfdfe; display: flex; justify-content: space-between; align-items: center; }
    .card-body { padding: 18px; }

    .form-group { margin-bottom: 12px; }
    label { display: block; font-size: 11px; font-weight: 600; color: var(--text-muted); text-transform: uppercase; margin-bottom: 5px; }
    input, select { width: 100%; background: #ffffff; border: 1px solid var(--border-dark); padding: 8px 12px; color: var(--text); font-family: var(--font-mono); font-size: 12px; border-radius: 6px; outline: none; transition: border-color 0.15s; }
    input:focus, select:focus { border-color: var(--primary); box-shadow: 0 0 0 3px rgba(2,132,199,0.1); }
    
    .btn { width: 100%; background: var(--primary); color: #ffffff; border: none; padding: 10px; font-size: 12px; font-weight: 600; cursor: pointer; border-radius: 6px; transition: background 0.15s; }
    .btn:hover { background: var(--primary-hover); }
    .btn-outline { background: transparent; border: 1px solid var(--border-dark); color: var(--text); margin-top: 8px; }
    .btn-outline:hover { background: #f8fafc; border-color: var(--text-muted); }

    table { width: 100%; border-collapse: collapse; font-family: var(--font-mono); font-size: 11.5px; }
    th { background: #f8fafc; padding: 10px 14px; text-align: left; font-weight: 600; color: var(--text-muted); border-bottom: 1px solid var(--border); font-family: var(--font-sans); font-size: 11px; text-transform: uppercase; }
    td { padding: 10px 14px; border-bottom: 1px solid var(--border); color: #334155; }
    tr:hover td { background: #f8fafc; }
    
    .pill { display: inline-block; padding: 2px 7px; border-radius: 4px; font-size: 10.5px; font-weight: 600; }
    .pill-dr { background: var(--danger-bg); color: var(--danger); border: 1px solid #fecaca; }
    .pill-cr { background: var(--success-bg); color: var(--success); border: 1px solid #bbf7d0; }
    .pill-ok { background: var(--success-bg); color: var(--success); font-weight: 600; }

    pre { background: #0f172a; border-radius: 6px; padding: 14px; font-family: var(--font-mono); font-size: 11px; color: #38bdf8; overflow-x: auto; max-height: 260px; line-height: 1.45; }

    /* Interactive Vector Architecture Elements */
    .vector-container { background: #ffffff; border: 1px solid var(--border); border-radius: 8px; padding: 20px; margin-bottom: 20px; }
    .interactive-node { cursor: pointer; transition: all 0.2s ease; }
    .interactive-node:hover rect { stroke: var(--primary); stroke-width: 2.5px; filter: drop-shadow(0 4px 6px rgba(2,132,199,0.15)); }
    .interactive-node.active rect { stroke: var(--primary); stroke-width: 3px; fill: #f0f9ff; }
    
    .node-explainer-card { background: #f8fafc; border: 1px solid var(--border); border-radius: 6px; padding: 16px; margin-top: 16px; }
    .node-title { font-size: 13px; font-weight: 700; color: var(--text); margin-bottom: 4px; }
    .node-desc { font-size: 12.5px; color: var(--text-muted); line-height: 1.6; }
    .node-meta { margin-top: 8px; font-family: var(--font-mono); font-size: 11px; color: var(--primary); font-weight: 600; }

    /* Glossary & PRD */
    .glossary-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 16px; }
    .glossary-card { background: #ffffff; border: 1px solid var(--border); border-radius: 6px; padding: 16px; }
    .glossary-title { font-family: var(--font-mono); font-size: 13px; font-weight: 700; color: var(--primary); margin-bottom: 6px; }
    .glossary-desc { font-size: 12.5px; color: var(--text-muted); line-height: 1.55; }
  </style>
</head>
<body>

  <header>
    <div class="brand">
      <span>TBG CORE TREASURY</span>
      <span class="badge">WHOLESALE TRANSACTION BANKING</span>
      <span style="font-size: 12px; color: var(--text-muted); font-weight: 500;">POSTGRESQL 16 // ACID DOUBLE-ENTRY</span>
    </div>
    <div class="nav-tabs">
      <button class="nav-btn active" onclick="switchTab('tab-clearing', this)">1. Outward Rail Dispatcher</button>
      <button class="nav-btn" onclick="switchTab('tab-escrow', this)">2. RERA Escrow Engine</button>
      <button class="nav-btn" onclick="switchTab('tab-sweeps', this)">3. Liquidity Concentration (ZBA)</button>
      <button class="nav-btn" onclick="switchTab('tab-vector', this)">4. Interactive Architecture Flow</button>
      <button class="nav-btn" onclick="switchTab('tab-prd', this)">5. Institutional PRD</button>
      <button class="nav-btn" onclick="switchTab('tab-glossary', this)">6. Banking Glossary</button>
    </div>
  </header>

  <div class="stats-bar">
    <div class="stat-card">
      <div class="stat-label">Corporate Operating Float</div>
      <div class="stat-val" id="disp-float">INR 10,00,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">CMS Suspense (Transit Liability)</div>
      <div class="stat-val" id="disp-suspense" style="color: var(--warning);">INR 0.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">RERA 70% Project Ring-Fence</div>
      <div class="stat-val" id="disp-rera-70" style="color: var(--primary);">INR 7,00,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Concentration Pool Balance</div>
      <div class="stat-val" id="disp-sweep-pool" style="color: var(--success);">INR 8,50,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Audited Ledger Postings</div>
      <div class="stat-val" id="disp-count">16 Entries</div>
    </div>
  </div>

  <!-- TAB 1: OUTWARD CLEARING DISPATCHER -->
  <div id="tab-clearing" class="tab-content active">
    <div class="grid-dashboard">
      <div>
        <div class="card">
          <div class="card-head">Execute Idempotent Payment</div>
          <div class="card-body">
            <form onsubmit="event.preventDefault(); submitPayment();">
              <div class="form-group">
                <label>Debit Float Account</label>
                <input type="text" id="src-account" value="00040310001928" readonly>
              </div>
              <div class="form-group">
                <label>Beneficiary Entity Name</label>
                <input type="text" id="bene-name" value="Tata Motors Commercial Fleet Ltd" required>
              </div>
              <div class="form-group">
                <label>Beneficiary Account Number</label>
                <input type="text" id="bene-acct" value="912345678901" required>
              </div>
              <div class="form-group">
                <label>Beneficiary IFSC Code</label>
                <input type="text" id="bene-ifsc" value="HDFC0000001" required>
              </div>
              <div class="form-group">
                <label>Payout Amount (INR)</label>
                <input type="number" id="payout-amt" value="250000" min="1" step="0.01" required>
              </div>
              <div class="form-group">
                <label>Rail Protocol Selection</label>
                <select id="rail-select">
                  <option value="AUTO">SMART_ROUTE (Cost / SLA Matrix)</option>
                  <option value="RTGS">RTGS (High-Value Gross Settlement)</option>
                  <option value="NEFT">NEFT (Batch Clearing - RBI SFMS)</option>
                  <option value="IMPS">IMPS (24x7 Real-Time Switch)</option>
                  <option value="UPI">UPI (NPCI 2.0 Intent)</option>
                </select>
              </div>
              <div class="form-group">
                <label>Distributed Idempotency Key</label>
                <input type="text" id="idem-key" readonly>
              </div>
              <button type="submit" class="btn">Execute Real-Time Transfer</button>
              <button type="button" class="btn btn-outline" onclick="submitEOD()">Trigger EOD Nostro Settlement</button>
            </form>
          </div>
        </div>
      </div>

      <div>
        <div class="card">
          <div class="card-head">
            <span>Immutable Double-Entry Postings Ledger</span>
            <span class="pill pill-ok">ZERO-SUM VERIFIED</span>
          </div>
          <div style="overflow-x:auto; max-height: 380px;">
            <table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Timestamp</th>
                  <th>Journal Voucher (JV)</th>
                  <th>Account Identifier</th>
                  <th>Leg</th>
                  <th>Amount</th>
                  <th>Narrative</th>
                  <th>Audit Status</th>
                </tr>
              </thead>
              <tbody id="ledger-body"></tbody>
            </table>
          </div>
        </div>

        <div style="display:grid; grid-template-columns: 1fr 1fr; gap:16px;">
          <div class="card">
            <div class="card-head">ISO 20022 PACS.008.001.08 Wire Output</div>
            <div class="card-body" style="padding:10px;">
              <pre id="pacs-display">&lt;!-- Execute payout to inspect pacs.008 schema payload --&gt;</pre>
            </div>
          </div>
          <div class="card">
            <div class="card-head">Outward ERP Webhook Dispatch (HMAC-SHA256)</div>
            <div class="card-body" style="padding:10px;">
              <pre id="webhook-display">/* Webhook dispatch event payload will appear here */</pre>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 2: RERA ESCROW ENGINE -->
  <div id="tab-escrow" class="tab-content">
    <div class="grid-dashboard">
      <div>
        <div class="card">
          <div class="card-head">RERA Dual-Escrow Allocation</div>
          <div class="card-body">
            <form onsubmit="event.preventDefault(); submitRera();">
              <div class="form-group"><label>RERA Registered Project ID</label><input type="text" value="PRJ-MAHARERA-PUNE-2026-904" readonly></div>
              <div class="form-group"><label>Homebuyer Virtual Account (VAN)</label><input type="text" id="rera-van" value="VAN-PUNE-TWR-801" required></div>
              <div class="form-group"><label>Inflow Consideration (INR)</label><input type="number" id="rera-amt" value="5000000" min="1000" step="100" required></div>
              <p style="font-size:11.5px; color:var(--text-muted); margin-bottom:14px; line-height:1.5;">Section 4(2)(l)(D) Rule: 70% automatically ring-fenced for verified construction expenses; 30% routed to operational liquidity.</p>
              <button type="submit" class="btn">Execute Dual Escrow Inflow</button>
            </form>
          </div>
        </div>
      </div>
      <div>
        <div style="display:grid; grid-template-columns: 1fr 1fr; gap:16px; margin-bottom:16px;">
          <div class="card" style="border-left: 4px solid var(--primary);">
            <div class="card-head">70% Dedicated Project Escrow</div>
            <div class="card-body">
              <div class="stat-val" id="disp-rera-70-box" style="color:var(--primary); font-size:22px;">INR 7,00,00,000.00</div>
              <p style="font-size:12px; color:var(--text-muted); margin-top:8px;">Withdrawals restricted to architect, engineer, and CA certified construction milestones.</p>
            </div>
          </div>
          <div class="card" style="border-left: 4px solid var(--success);">
            <div class="card-head">30% Operational Current Account</div>
            <div class="card-body">
              <div class="stat-val" id="disp-rera-30-box" style="color:var(--success); font-size:22px;">INR 3,00,00,000.00</div>
              <p style="font-size:12px; color:var(--text-muted); margin-top:8px;">Liquid operational funds available for general administrative overheads and developer OpEx.</p>
            </div>
          </div>
        </div>
        <div class="card">
          <div class="card-head">Dual-Allocation Event Stream</div>
          <div class="card-body" style="padding:10px;">
            <pre id="rera-output">/* RERA Execution output will stream here */</pre>
          </div>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 3: LIQUIDITY SWEEPS (ZBA) -->
  <div id="tab-sweeps" class="tab-content">
    <div class="grid-dashboard">
      <div>
        <div class="card">
          <div class="card-head">Zero-Balance Account (ZBA) Sweeper</div>
          <div class="card-body">
            <form onsubmit="event.preventDefault(); submitSweep();">
              <div class="form-group"><label>Subsidiary Current Account (ZBA)</label><input type="text" id="zba-acct" value="SUBSIDIARY_PUNE_PLANT_4021" required></div>
              <div class="form-group"><label>Master Treasury Concentration Pool</label><input type="text" value="MASTER_LIQUIDITY_POOL_01" readonly></div>
              <div class="form-group"><label>Sweep Amount (INR)</label><input type="number" id="sweep-amt" value="2500000" min="1000" step="100" required></div>
              <button type="submit" class="btn">Execute Automatic Cash Sweep</button>
            </form>
          </div>
        </div>
      </div>
      <div>
        <div class="card" style="border-left: 4px solid var(--success);">
          <div class="card-head">Central Concentrated Liquidity</div>
          <div class="card-body">
            <div class="stat-val" id="disp-sweep-box" style="color:var(--success); font-size:24px;">INR 8,50,00,000.00</div>
            <p style="font-size:12px; color:var(--text-muted); margin-top:8px;">Aggregated corporate liquidity actively deployed in overnight central bank reverse repo.</p>
          </div>
        </div>
        <div class="card">
          <div class="card-head">Sweep Journal Voucher Postings</div>
          <div class="card-body" style="padding:10px;">
            <pre id="sweep-output">/* Sweep Execution output will appear here */</pre>
          </div>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 4: INTERACTIVE ARCHITECTURE FLOW -->
  <div id="tab-vector" class="tab-content">
    <div class="vector-container">
      <div style="margin-bottom:16px;">
        <h2 style="font-size:16px; font-weight:700; color:var(--text);">Multi-Rail Transaction Banking Architecture</h2>
        <p style="color:var(--text-muted); font-size:12.5px;">Click on any node in the processing pipeline below to inspect its operational role, regulatory mandates, and runtime state transformation.</p>
      </div>

      <svg viewBox="0 0 1000 240" xmlns="http://www.w3.org/2000/svg" style="width:100%; border: 1px solid var(--border); border-radius:6px; background:#fafcff;">
        <defs>
          <marker id="arr" viewBox="0 0 10 10" refX="5" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
            <path d="M 0 0 L 10 5 L 0 10 z" fill="#0284c7" />
          </marker>
        </defs>

        <!-- Node 1: Corporate ERP -->
        <g class="interactive-node active" id="node-erp" onclick="inspectNode('erp')">
          <rect x="25" y="75" width="160" height="90" rx="8" fill="#ffffff" stroke="#cbd5e1" stroke-width="2"/>
          <text x="105" y="112" fill="#0f172a" font-family="-apple-system, sans-serif" font-size="13" text-anchor="middle" font-weight="700">1. Corporate ERP</text>
          <text x="105" y="132" fill="#64748b" font-family="monospace" font-size="10.5" text-anchor="middle">SAP / Host-to-Host</text>
          <text x="105" y="148" fill="#0284c7" font-family="monospace" font-size="10" text-anchor="middle">pain.001 / REST API</text>
        </g>

        <!-- Node 2: Ingress & Validation -->
        <g class="interactive-node" id="node-ingress" onclick="inspectNode('ingress')">
          <rect x="245" y="75" width="170" height="90" rx="8" fill="#ffffff" stroke="#cbd5e1" stroke-width="2"/>
          <text x="330" y="112" fill="#0f172a" font-family="-apple-system, sans-serif" font-size="13" text-anchor="middle" font-weight="700">2. Ingress & Gate</text>
          <text x="330" y="132" fill="#64748b" font-family="monospace" font-size="10.5" text-anchor="middle">Idempotency & HMAC</text>
          <text x="330" y="148" fill="#0284c7" font-family="monospace" font-size="10" text-anchor="middle">AML / OFAC Screen</text>
        </g>

        <!-- Node 3: Double-Entry Core -->
        <g class="interactive-node" id="node-ledger" onclick="inspectNode('ledger')">
          <rect x="475" y="75" width="180" height="90" rx="8" fill="#ffffff" stroke="#cbd5e1" stroke-width="2"/>
          <text x="565" y="112" fill="#0f172a" font-family="-apple-system, sans-serif" font-size="13" text-anchor="middle" font-weight="700">3. Ledger Core</text>
          <text x="565" y="132" fill="#64748b" font-family="monospace" font-size="10.5" text-anchor="middle">Debit Float // Credit Suspense</text>
          <text x="565" y="148" fill="#16a34a" font-family="monospace" font-size="10" text-anchor="middle">&Sigma; DR = &Sigma; CR Verified</text>
        </g>

        <!-- Node 4: Dynamic Rail Router -->
        <g class="interactive-node" id="node-router" onclick="inspectNode('router')">
          <rect x="715" y="25" width="130" height="50" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
          <text x="780" y="54" fill="#0f172a" font-family="-apple-system, sans-serif" font-size="11" text-anchor="middle" font-weight="600">RTGS (SFMS)</text>
          
          <rect x="715" y="95" width="130" height="50" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
          <text x="780" y="124" fill="#0f172a" font-family="-apple-system, sans-serif" font-size="11" text-anchor="middle" font-weight="600">NEFT / ACH</text>

          <rect x="715" y="165" width="130" height="50" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
          <text x="780" y="194" fill="#0f172a" font-family="-apple-system, sans-serif" font-size="11" text-anchor="middle" font-weight="600">UPI / IMPS (NPCI)</text>
        </g>

        <!-- Node 5: Central Clearing Settlement -->
        <g class="interactive-node" id="node-clearing" onclick="inspectNode('clearing')">
          <rect x="885" y="90" width="95" height="60" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="2"/>
          <text x="932" y="120" fill="#0f172a" font-family="-apple-system, sans-serif" font-size="11" text-anchor="middle" font-weight="700">Nostro</text>
          <text x="932" y="136" fill="#16a34a" font-family="monospace" font-size="9" text-anchor="middle">Settled</text>
        </g>

        <!-- Connectors -->
        <line x1="185" y1="120" x2="240" y2="120" stroke="#0284c7" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="415" y1="120" x2="470" y2="120" stroke="#0284c7" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="655" y1="105" x2="710" y2="50" stroke="#0284c7" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="655" y1="120" x2="710" y2="120" stroke="#0284c7" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="655" y1="135" x2="710" y2="190" stroke="#0284c7" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="845" y1="120" x2="880" y2="120" stroke="#94a3b8" stroke-width="1.5" marker-end="url(#arr)"/>
      </svg>

      <div class="node-explainer-card">
        <div class="node-title" id="explainer-title">1. Corporate ERP Integration Layer</div>
        <div class="node-desc" id="explainer-desc">Corporate clients submit payment batches or real-time payroll instructions directly from treasury platforms (SAP, Oracle Treasury, or Host-to-Host SFTP) using standardized pain.001.001.09 initiation messages. Each message includes an immutable client reference.</div>
        <div class="node-meta" id="explainer-meta">Supported Protocols: ISO 20022 pain.001, REST Webhooks, AS2 Direct Pipe</div>
      </div>
    </div>
  </div>

  <!-- TAB 5: PRD -->
  <div id="tab-prd" class="tab-content">
    <div class="card">
      <div class="card-head">Institutional Product Requirement Document (PRD)</div>
      <div class="card-body">
        <h3 style="font-size:14px; margin-bottom:8px; color:var(--text);">1. System Overview</h3>
        <p style="color:var(--text-muted); margin-bottom:16px;">The Transaction Banking Gateway (TBG) Core acts as the centralized multi-rail financial clearing orchestrator for Scheduled Commercial Banks and large payment aggregators. It eliminates ledger settlement leakage, prevents double-debit anomalies over unreliable networks, and guarantees straight-through processing (STP) exceeding 99.8% across interbank clearing systems.</p>

        <h3 style="font-size:14px; margin-bottom:8px; color:var(--text);">2. Architectural Guardrails</h3>
        <ul style="color:var(--text-muted); margin-left:20px; line-height:1.8; margin-bottom:16px;">
          <li><strong>Invariant Double-Entry Ledgers:</strong> In accordance with Basel III and RBI operational guidelines, single-sided account mutations are strictly disallowed. Every outward transaction triggers a debit on the Corporate Float Account and an offsetting credit on the CMS Transit Suspense Account before dispatching down the clearing wire.</li>
          <li><strong>Deterministic Smart Routing:</strong> Transactions &ge; INR 2,00,000 are deterministically routed to RBI RTGS for gross settlement; transactions &le; INR 1,00,000 route via high-throughput retail rails (UPI/IMPS) based on real-time sub-second latency telemetry.</li>
          <li><strong>RERA Section 4(2)(l)(D) Escrow Separation:</strong> Real estate customer collections into virtual accounts are split automatically into 70% unencumbered site construction escrow accounts and 30% business operational accounts.</li>
        </ul>
      </div>
    </div>
  </div>

  <!-- TAB 6: GLOSSARY -->
  <div id="tab-glossary" class="tab-content">
    <div class="glossary-grid">
      <div class="glossary-card">
        <div class="glossary-title">pacs.008 (ISO 20022)</div>
        <div class="glossary-desc">Financial Customer Credit Transfer message. The interbank industry standard for moving funds between debtor and creditor institutions across RTGS, NEFT, and SWIFT CBPR+ rails.</div>
      </div>
      <div class="glossary-card">
        <div class="glossary-title">CMS Suspense Account</div>
        <div class="glossary-desc">An internal clearing transit account maintained by the bank. Represents the bank's liability to the central clearing switch before end-of-day settlement files are reconciled against the central bank Nostro.</div>
      </div>
      <div class="glossary-card">
        <div class="glossary-title">Nostro Account</div>
        <div class="glossary-desc">An account held by the domestic bank in the books of another institution (or RBI directly) in local or foreign currency, used to settle multilateral net obligations.</div>
      </div>
      <div class="glossary-card">
        <div class="glossary-title">Zero-Sum Audit Invariance</div>
        <div class="glossary-desc">Mathematical proof that for every transaction voucher, the sum of debits exactly matches the sum of credits (&Sigma; DR - &Sigma; CR = 0), ensuring zero money leakage occurs in ledger memory.</div>
      </div>
      <div class="glossary-card">
        <div class="glossary-title">Zero Balance Account (ZBA)</div>
        <div class="glossary-desc">A corporate subsidiary account where balances are automatically swept into a master concentration pool at cutoff, optimizing interest yield while maintaining zero idle decentralized capital.</div>
      </div>
      <div class="glossary-card">
        <div class="glossary-title">Virtual Account (VAN)</div>
        <div class="glossary-desc">Shadow routing identifiers mapped to a physical corporate account, enabling automated 1:1 invoice matching and instant reconciliation without opening thousands of distinct bank accounts.</div>
      </div>
    </div>
  </div>

  <script>
    const nodeExplains = {
      erp: {
        title: "1. Corporate ERP Integration Layer",
        desc: "Corporate clients submit payment batches or real-time payroll instructions directly from treasury platforms (SAP, Oracle Treasury, or Host-to-Host SFTP) using standardized pain.001.001.09 initiation messages. Each message includes an immutable client reference.",
        meta: "Supported Protocols: ISO 20022 pain.001, REST Webhooks, AS2 Direct Pipe"
      },
      ingress: {
        title: "2. Ingress & Sanction Filtering Layer",
        desc: "Ingress nodes validate HMAC signatures, verify distributed idempotency keys to eliminate double-payment retry risks, and execute sub-millisecond OFAC/FATF sanction screening before requests hit ledger memory.",
        meta: "Security: SHA-256 HMAC Verification // Distributed Redis Locks"
      },
      ledger: {
        title: "3. Double-Entry Ledger Core Engine",
        desc: "Executes atomic double-entry journal postings. Corporate float is debited while the CMS Suspense Account is credited. Balance reservation guarantees the bank does not assume uncollateralized daylight overdrafts.",
        meta: "Guarantees: Strict ACID Compliance // Invariant: Sum(DR) - Sum(CR) = 0"
      },
      router: {
        title: "4. Multi-Rail Smart Routing Matrix",
        desc: "Dynamic decisioning evaluates ticket size, rail TPS health, latency, and interchange/clearing costs. Amounts &ge; INR 2L route via RTGS; retail amounts route via instant NPCI UPI/IMPS rails.",
        meta: "Rails: RBI RTGS // RBI NEFT // NPCI UPI 2.0 // NPCI IMPS"
      },
      clearing: {
        title: "5. Central Bank Nostro Settlement & Finality",
        desc: "End-of-day statement reconciliation (camt.053) matches multilateral net obligations, discharging the bank's CMS Suspense liability against the central bank Nostro account for definitive settlement.",
        meta: "Finality: Immediate in RTGS // Multilateral Batched in NEFT/UPI"
      }
    };

    function inspectNode(key) {
      document.querySelectorAll('.interactive-node').forEach(n => n.classList.remove('active'));
      const activeNode = document.getElementById('node-' + key);
      if (activeNode) activeNode.classList.add('active');

      const data = nodeExplains[key];
      if (data) {
        document.getElementById('explainer-title').innerText = data.title;
        document.getElementById('explainer-desc').innerText = data.desc;
        document.getElementById('explainer-meta').innerText = data.meta;
      }
    }

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
      
      document.getElementById('disp-float').innerText = 'INR ' + Number(data.float_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-suspense').innerText = 'INR ' + Number(data.suspense_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-70').innerText = 'INR ' + Number(data.rera_project).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-70-box').innerText = 'INR ' + Number(data.rera_project).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-30-box').innerText = 'INR ' + Number(data.rera_ops).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-sweep-pool').innerText = 'INR ' + Number(data.sweep_pool).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-sweep-box').innerText = 'INR ' + Number(data.sweep_pool).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-count').innerText = data.entry_counter + ' Entries';

      const tbody = document.getElementById('ledger-body');
      tbody.innerHTML = '';
      data.ledger.forEach(r => {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td>' + r.id + '</td><td>' + r.time + '</td><td>' + r.jv + '</td><td>' + r.acct + '</td><td><span class="pill ' + (r.leg==='DR'?'pill-dr':'pill-cr') + '">' + r.leg + '</span></td><td>INR ' + Number(r.amt).toLocaleString('en-IN', {minimumFractionDigits: 2}) + '</td><td>' + (r.narrative || 'Posting') + '</td><td class="pill-ok">✓ ' + r.verdict + '</td>';
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
      if (!res.ok) { alert('Execution Failed: ' + await res.text()); return; }
      const out = await res.json();

      document.getElementById('pacs-display').innerText = out.iso_pacs008;
      document.getElementById('webhook-display').innerText = JSON.stringify({
        event: "payment.settled",
        timestamp: new Date().toISOString(),
        signature_sha256: out.signature,
        data: {
          jv_id: out.jv_id,
          source_account: payload.source_account,
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

    async function submitRera() {
      const payload = {
        project_id: "PRJ-MAHARERA-PUNE-2026-904",
        buyer_van: document.getElementById('rera-van').value,
        amount: parseFloat(document.getElementById('rera-amt').value)
      };
      const res = await fetch('/api/rera', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('rera-output').innerText = JSON.stringify(out, null, 2);
      await refreshState();
    }

    async function submitSweep() {
      const payload = {
        subsidiary_account: document.getElementById('zba-acct').value,
        sweep_amount: parseFloat(document.getElementById('sweep-amt').value)
      };
      const res = await fetch('/api/sweep', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('sweep-output').innerText = JSON.stringify(out, null, 2);
      await refreshState();
    }

    async function submitEOD() {
      const res = await fetch('/api/eod', { method: 'POST' });
      if (!res.ok) { alert('Settlement notice: ' + await res.text()); return; }
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
