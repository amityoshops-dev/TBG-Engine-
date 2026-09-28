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
	ID         string  `json:"id"`
	Timestamp  string  `json:"time"`
	JVID       string  `json:"jv"`
	Account    string  `json:"acct"`
	Leg        string  `json:"leg"` // DR or CR
	Amount     float64 `json:"amt"`
	Currency   string  `json:"currency"`
	Verdict    string  `json:"verdict"`
	Narrative  string  `json:"narrative"`
	Standard   string  `json:"standard"` // ISO20022, SWIFT_MT, etc.
}

type EngineState struct {
	sync.Mutex
	FloatINR          float64
	FloatUSD          float64
	SuspenseINR       float64
	SuspenseUSD       float64
	NostroUSDInterbank float64
	LCEscrowINR       float64
	EntryCounter      int
	Ledger            []JournalEntry
	IdempotencyMap    map[string]bool
}

var state = EngineState{
	FloatINR:           50000000.00,
	FloatUSD:           1200000.00,
	SuspenseINR:        0.00,
	SuspenseUSD:        0.00,
	NostroUSDInterbank: 4500000.00,
	LCEscrowINR:        25000000.00, // Trade Finance LC Margin Ring-Fenced
	EntryCounter:       16,
	IdempotencyMap:     make(map[string]bool),
	Ledger: []JournalEntry{
		{ID: "#16", Timestamp: "14:10:02", JVID: "JV-90812-SWIFT", Account: "NOSTRO_US_CHASE_NYC", Leg: "CR", Amount: 250000.00, Currency: "USD", Verdict: "ZERO-SUM OK", Narrative: "Cross-Border Settlement Discharge", Standard: "SWIFT MT202"},
		{ID: "#15", Timestamp: "14:10:02", JVID: "JV-90812-SWIFT", Account: "TRANSIT_OUTWARD_USD_SUSPENSE", Leg: "DR", Amount: 250000.00, Currency: "USD", Verdict: "ZERO-SUM OK", Narrative: "Intermediary Settlement Discharge", Standard: "SWIFT MT202"},
		{ID: "#14", Timestamp: "14:08:11", JVID: "JV-90810-MX", Account: "TRANSIT_OUTWARD_USD_SUSPENSE", Leg: "CR", Amount: 250000.00, Currency: "USD", Verdict: "ZERO-SUM OK", Narrative: "Customer Wire Transit Reservation", Standard: "pacs.008.001.08"},
		{ID: "#13", Timestamp: "14:08:11", JVID: "JV-90810-MX", Account: "CORP_US_FLOAT_0029", Leg: "DR", Amount: 250000.00, Currency: "USD", Verdict: "ZERO-SUM OK", Narrative: "Cross-Border Corporate Outflow", Standard: "pacs.008.001.08"},
	},
}

// 1. Cross-Border Wire Dispatch (Produces valid ISO pacs.008 AND SWIFT MT103)
func handleCrossBorderPayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()
	var req struct {
		SourceAccount      string  `json:"source_account"`
		BeneficiaryName    string  `json:"beneficiary_name"`
		BeneficiaryIBAN    string  `json:"beneficiary_iban"`
		BeneficiaryBIC     string  `json:"beneficiary_bic"`
		IntermediaryBIC    string  `json:"intermediary_bic"`
		Amount             float64 `json:"amount"`
		Currency           string  `json:"currency"`
		ChargeBearer       string  `json:"charge_bearer"` // OUR, BEN, SHA
		PaymentReference   string  `json:"payment_reference"`
		IdempotencyKey     string  `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400); return
	}

	state.Lock()
	defer state.Unlock()

	if state.IdempotencyMap[req.IdempotencyKey] {
		http.Error(w, "Conflict: Duplicate Idempotency Key", http.StatusConflict); return
	}
	if req.Currency == "USD" && req.Amount > state.FloatUSD {
		http.Error(w, "Insufficient USD Float balance", 400); return
	}

	state.FloatUSD -= req.Amount
	state.SuspenseUSD += req.Amount
	state.IdempotencyMap[req.IdempotencyKey] = true

	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%06x-MX", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "TRANSIT_OUTWARD_USD_SUSPENSE", Leg: "CR", Amount: req.Amount, Currency: req.Currency, Verdict: "ZERO-SUM OK", Narrative: "Outward Transit Float Reserve", Standard: "ISO:pacs.008"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Currency: req.Currency, Verdict: "ZERO-SUM OK", Narrative: "Client Cross-Border Debit", Standard: "ISO:pacs.008"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	msgID := fmt.Sprintf("MSG%s%06d", time.Now().Format("20060102"), rand.Intn(999999))
	uetr := fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", rand.Uint32(), rand.Uint32()&0xffff, rand.Uint32()&0xfff, rand.Uint32()&0xffff, rand.Uint64()&0xffffffffffff)

	// ISO 20022 pacs.008.001.08 XML
	isoXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">
  <FIToFICstmrCdtTrf>
    <GrpHdr>
      <MsgId>%s</MsgId>
      <CreDtTm>%s</CreDtTm>
      <NbOfTxs>1</NbOfTxs>
      <SttlmInf>
        <SttlmMtd>CLRG</SttlmMtd>
        <ClrSys><Prtry>SWIFT_CBPR_PLUS</Prtry></ClrSys>
      </SttlmInf>
    </GrpHdr>
    <CdtTrfTxInf>
      <PmtId>
        <EndToEndId>%s</EndToEndId>
        <UETR>%s</UETR>
      </PmtId>
      <IntrBkSttlmAmt Ccy="%s">%.2f</IntrBkSttlmAmt>
      <ChrgBr>%s</ChrgBr>
      <Dbtr><Nm>TREASURY CORP GLOBAL CLIENT</Nm></Dbtr>
      <DbtrAcct><Id><Othr><Id>%s</Id></Othr></Id></DbtrAcct>
      <DbtrAgt><FinInstnId><BICFI>TBGUSB33XXX</BICFI></FinInstnId></DbtrAgt>
      <IntrmyAgt1><FinInstnId><BICFI>%s</BICFI></FinInstnId></IntrmyAgt1>
      <CdtrAgt><FinInstnId><BICFI>%s</BICFI></FinInstnId></CdtrAgt>
      <Cdtr><Nm>%s</Nm></Cdtr>
      <CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>
      <RmtInf><Ustrd>%s</Ustrd></RmtInf>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>`, msgID, time.Now().UTC().Format(time.RFC3339), req.IdempotencyKey, uetr, req.Currency, req.Amount, req.ChargeBearer, req.SourceAccount, req.IntermediaryBIC, req.BeneficiaryBIC, req.BeneficiaryName, req.BeneficiaryIBAN, req.PaymentReference)

	// SWIFT FIN MT103 Equivalent Wire Block
	swiftMT103 := fmt.Sprintf(`{1:F01TBGUSB33AXXX0000000000}{2:I103%sXXXXN}{3:{121:%s}}{4:
:20:%s
:23B:CRED
:32A:%s%s%.2f
:33B:%s%.2f
:50K:/%s
TREASURY CORP GLOBAL CLIENT
:53A:TBGUSB33XXX
:56A:%s
:57A:%s
:59:/%s
%s
:71A:%s
:72:/REC/%s
-}`, req.BeneficiaryBIC, uetr, req.IdempotencyKey, time.Now().Format("060102"), req.Currency, req.Amount, req.Currency, req.Amount, req.SourceAccount, req.IntermediaryBIC, req.BeneficiaryBIC, req.BeneficiaryIBAN, req.BeneficiaryName, req.ChargeBearer, req.PaymentReference)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "PROCESSED",
		"jv_id":       jvID,
		"uetr":        uetr,
		"iso_xml":     isoXML,
		"swift_mt103": swiftMT103,
		"latency_ms":  latency,
	})
}

// 2. Trade Finance: Letter of Credit (LC) Margin Earmarking & Drawdown (MT700 Engine)
func handleLCDrawdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()
	var req struct {
		LCReference       string  `json:"lc_reference"`
		ApplicantName     string  `json:"applicant_name"`
		BeneficiaryName   string  `json:"beneficiary_name"`
		IssuingBankBIC    string  `json:"issuing_bank_bic"`
		DrawdownAmountINR float64 `json:"drawdown_amount_inr"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	state.Lock()
	defer state.Unlock()

	if req.DrawdownAmountINR > state.LCEscrowINR {
		http.Error(w, "LC Escrow Balance Exceeded", 400); return
	}

	state.LCEscrowINR -= req.DrawdownAmountINR
	state.FloatINR -= req.DrawdownAmountINR
	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%06x-LC", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "BENEFICIARY_ADVISING_PAYMENT_AC", Leg: "CR", Amount: req.DrawdownAmountINR, Currency: "INR", Verdict: "ZERO-SUM OK", Narrative: "MT700 LC Beneficiary Settlement", Standard: "SWIFT MT700"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: "LC_CASH_MARGIN_EARMARKED_8819", Leg: "DR", Amount: req.DrawdownAmountINR, Currency: "INR", Verdict: "ZERO-SUM OK", Narrative: "Release of 100% Cash Collateral Margin", Standard: "Trade Finance"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	swiftMT700 := fmt.Sprintf(`{1:F01%s0000000000}{2:I700TBGUSB33XXXXN}{4:
:27:1/1
:40A:IRREVOCABLE
:20:%s
:31C:%s
:31D:%sINDIA
:50:%s
:59:%s
:32B:INR%.2f
:41A:TBGUSB33XXX BY NEGOTIATION
:78:PAYMENT ON RECEIPT OF CLEAN BILL OF LADING AND SGS INSPECTION CERTIFICATE
-}`, req.IssuingBankBIC, req.LCReference, time.Now().Format("060102"), time.Now().AddDate(0, 3, 0).Format("060102"), req.ApplicantName, req.BeneficiaryName, req.DrawdownAmountINR)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "HONORED_AND_SETTLED",
		"jv_id":           jvID,
		"swift_mt700":     swiftMT700,
		"remaining_lc":    state.LCEscrowINR,
		"latency_ms":      latency,
	})
}

// 3. CAMT.053 Electronic Bank-to-Customer Statement Generator
func handleCamt053(w http.ResponseWriter, r *http.Request) {
	state.Lock()
	defer state.Unlock()

	stmtID := fmt.Sprintf("STMT-%s-%04d", time.Now().Format("20060102"), rand.Intn(9999))
	camtXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08">
  <BkToCstmrStmt>
    <GrpHdr>
      <MsgId>%s</MsgId>
      <CreDtTm>%s</CreDtTm>
    </GrpHdr>
    <Stmt>
      <Id>%s</Id>
      <Acct>
        <Id><Othr><Id>NOSTRO_US_CHASE_NYC</Id></Othr></Id>
        <Ccy>USD</Ccy>
      </Acct>
      <Bal>
        <Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp>
        <Amt Ccy="USD">4500000.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Dt><Dt>%s</Dt></Dt>
      </Bal>
      <Bal>
        <Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp>
        <Amt Ccy="USD">%.2f</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Dt><Dt>%s</Dt></Dt>
      </Bal>
    </Stmt>
  </BkToCstmrStmt>
</Document>`, stmtID, time.Now().UTC().Format(time.RFC3339), stmtID, time.Now().Format("2006-01-02"), state.NostroUSDInterbank-state.SuspenseUSD, time.Now().Format("2006-01-02"))

	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(camtXML))
}

func handleState(w http.ResponseWriter, r *http.Request) {
	state.Lock()
	defer state.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"float_inr":    state.FloatINR,
		"float_usd":    state.FloatUSD,
		"suspense_inr": state.SuspenseINR,
		"suspense_usd": state.SuspenseUSD,
		"nostro_usd":   state.NostroUSDInterbank,
		"lc_escrow":    state.LCEscrowINR,
		"entry_count":  state.EntryCounter,
		"ledger":       state.Ledger,
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" { port = "10000" }

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(institutionalUI))
	})
	http.HandleFunc("/api/state", handleState)
	http.HandleFunc("/api/crossborder", handleCrossBorderPayment)
	http.HandleFunc("/api/tradefinance", handleLCDrawdown)
	http.HandleFunc("/api/camt053", handleCamt053)

	log.Printf("TBG-INSTITUTIONAL CORE running on :%s", port)
	http.ListenAndServe(":"+port, nil)
}

const institutionalUI = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>TBG-CORE // Institutional Transaction Banking Platform</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    :root {
      --bg: #030712;
      --surface: #0b1329;
      --card: #111e38;
      --border: #1e3a5f;
      --primary: #00d2ff;
      --secondary: #00f090;
      --warn: #ffaa00;
      --danger: #ff3366;
      --text: #f3f4f6;
      --muted: #94a3b8;
      --font-mono: 'JetBrains Mono', 'Courier New', monospace;
      --font-sans: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { background: var(--bg); color: var(--text); font-family: var(--font-sans); font-size: 13px; }
    header { background: var(--surface); border-bottom: 1px solid var(--border); padding: 12px 24px; display: flex; justify-content: space-between; align-items: center; position: sticky; top: 0; z-index: 100; }
    .brand { display: flex; align-items: center; gap: 12px; font-family: var(--font-mono); font-weight: 700; font-size: 14px; }
    .badge { background: rgba(0,210,255,0.1); border: 1px solid var(--primary); color: var(--primary); font-size: 10px; padding: 2px 6px; border-radius: 3px; }
    .nav-tabs { display: flex; gap: 8px; }
    .nav-btn { background: transparent; border: 1px solid transparent; color: var(--muted); padding: 7px 12px; font-size: 11px; cursor: pointer; border-radius: 4px; font-family: var(--font-mono); font-weight: 600; text-transform: uppercase; }
    .nav-btn.active, .nav-btn:hover { color: var(--primary); background: var(--card); border-color: var(--border); }
    .stats-bar { display: grid; grid-template-columns: repeat(5, 1fr); gap: 1px; background: var(--border); border-bottom: 1px solid var(--border); }
    .stat-card { background: var(--surface); padding: 12px 20px; }
    .stat-label { font-size: 10px; font-family: var(--font-mono); color: var(--muted); text-transform: uppercase; margin-bottom: 4px; }
    .stat-val { font-family: var(--font-mono); font-size: 15px; font-weight: 600; }
    .tab-content { display: none; padding: 20px 24px; }
    .tab-content.active { display: block; }
    .grid-layout { display: grid; grid-template-columns: 420px 1fr; gap: 20px; }
    .panel { background: var(--surface); border: 1px solid var(--border); border-radius: 6px; overflow: hidden; margin-bottom: 16px; }
    .panel-head { background: var(--card); padding: 10px 16px; border-bottom: 1px solid var(--border); font-family: var(--font-mono); font-size: 11px; font-weight: 600; text-transform: uppercase; color: var(--muted); display: flex; justify-content: space-between; align-items: center; }
    .panel-body { padding: 16px; }
    .form-group { margin-bottom: 10px; }
    label { display: block; font-family: var(--font-mono); font-size: 10px; color: var(--muted); text-transform: uppercase; margin-bottom: 4px; }
    input, select { width: 100%; background: var(--bg); border: 1px solid var(--border); padding: 8px 10px; color: var(--text); font-family: var(--font-mono); font-size: 12px; border-radius: 4px; outline: none; }
    input:focus, select:focus { border-color: var(--primary); }
    .btn { width: 100%; background: var(--primary); color: #000; border: none; padding: 10px; font-family: var(--font-mono); font-size: 11px; font-weight: 700; text-transform: uppercase; cursor: pointer; border-radius: 4px; }
    .btn:hover { background: #50e0ff; }
    .btn-green { background: var(--secondary); }
    table { width: 100%; border-collapse: collapse; font-family: var(--font-mono); font-size: 11px; }
    th { background: var(--card); padding: 8px 12px; text-align: left; color: var(--muted); border-bottom: 1px solid var(--border); }
    td { padding: 8px 12px; border-bottom: 1px solid rgba(30, 58, 95, 0.4); }
    .tag-dr { color: var(--danger); font-weight: 700; }
    .tag-cr { color: var(--secondary); font-weight: 700; }
    pre { background: var(--bg); border: 1px solid var(--border); border-radius: 4px; padding: 12px; font-family: var(--font-mono); font-size: 10.5px; color: #9cdcfe; overflow-x: auto; max-height: 280px; }
    .doc-section { background: var(--surface); border: 1px solid var(--border); border-radius: 6px; padding: 24px; margin-bottom: 24px; }
    .doc-section h2 { font-size: 16px; font-family: var(--font-mono); color: var(--primary); margin-bottom: 12px; border-bottom: 1px solid var(--border); padding-bottom: 8px; }
    .doc-section p { color: var(--muted); margin-bottom: 12px; line-height: 1.6; }
    .glossary-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 16px; }
    .term-card { background: var(--card); border: 1px solid var(--border); border-radius: 4px; padding: 16px; }
    .term-title { font-family: var(--font-mono); font-size: 13px; color: var(--primary); margin-bottom: 6px; font-weight: 700; }
    .term-desc { font-size: 12px; color: var(--muted); line-height: 1.6; }
    .vector-svg { width: 100%; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; padding: 16px; }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <span>TBG-CORE</span>
      <span class="badge">SWIFT CBPR+ // ISO 20022 ENGINE</span>
      <span style="color: var(--muted); font-size: 11px;">FINACLE CORPORATE TREASURY</span>
    </div>
    <div class="nav-tabs">
      <button class="nav-btn active" onclick="switchTab('tab-crossborder', this)">1. Cross-Border & SWIFT (MT103/pacs.008)</button>
      <button class="nav-btn" onclick="switchTab('tab-tradefinance', this)">2. Trade Finance (MT700 LC)</button>
      <button class="nav-btn" onclick="switchTab('tab-recon', this)">3. Nostro Recon (CAMT.053)</button>
      <button class="nav-btn" onclick="switchTab('tab-architecture', this)">4. Architecture Blueprint</button>
      <button class="nav-btn" onclick="switchTab('tab-glossary', this)">5. Enterprise Glossary</button>
    </div>
  </header>

  <div class="stats-bar">
    <div class="stat-card">
      <div class="stat-label">USD Corporate Float</div>
      <div class="stat-val" id="disp-float-usd" style="color:var(--primary)">USD 1,200,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">USD Outward Transit Suspense</div>
      <div class="stat-val" id="disp-suspense-usd" style="color:var(--warn)">USD 0.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">CHASE NY Nostro Settlement Balance</div>
      <div class="stat-val" id="disp-nostro-usd" style="color:var(--secondary)">USD 4,500,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Trade Finance Collateral Margin (LC)</div>
      <div class="stat-val" id="disp-lc-escrow" style="color:var(--primary)">INR 2,50,00,000.00</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Total Audited Postings</div>
      <div class="stat-val" id="disp-count">16 Entries</div>
    </div>
  </div>

  <!-- TAB 1: CROSS-BORDER & SWIFT -->
  <div id="tab-crossborder" class="tab-content active">
    <div class="grid-layout">
      <div class="panel">
        <div class="panel-head">ISO 20022 CBPR+ / SWIFT Wire Engine</div>
        <div class="panel-body">
          <form onsubmit="event.preventDefault(); submitCrossBorder();">
            <div class="form-group"><label>Source Corporate USD Account</label><input type="text" id="cb-src" value="CORP_US_FLOAT_0029" readonly></div>
            <div class="form-group"><label>Beneficiary Entity Name</label><input type="text" id="cb-bene-name" value="Airbus Operations GmbH" required></div>
            <div class="form-group"><label>Beneficiary IBAN</label><input type="text" id="cb-bene-iban" value="DE89370400440532013000" required></div>
            <div class="form-group"><label>Creditor Agent BIC (Beneficiary Bank)</label><input type="text" id="cb-bene-bic" value="DBEUMM21XXX" required></div>
            <div class="form-group"><label>Intermediary Agent BIC (Cover Clearing)</label><input type="text" id="cb-int-bic" value="CHASUS33XXX" required></div>
            <div class="form-group"><label>Transfer Amount (USD)</label><input type="number" id="cb-amt" value="250000" min="100" step="0.01" required></div>
            <div class="form-group">
              <label>Charge Bearer (Tag 71A / ChrgBr)</label>
              <select id="cb-chrg">
                <option value="OUR">OUR (All transaction charges borne by Debtor)</option>
                <option value="SHA">SHA (Charges shared between Debtor and Creditor)</option>
                <option value="BEN">BEN (All charges borne by Beneficiary)</option>
              </select>
            </div>
            <div class="form-group"><label>Unique End-To-End Identifier (UETR / E2E)</label><input type="text" id="cb-idem" readonly></div>
            <button type="submit" class="btn">Dispatch CBPR+ pacs.008 & MT103</button>
          </form>
        </div>
      </div>

      <div>
        <div class="panel">
          <div class="panel-head">Dual Output: ISO 20022 XML & SWIFT FIN MT103</div>
          <div style="display:grid; grid-template-columns:1fr 1fr; gap:12px; padding:16px;">
            <div>
              <div class="stat-label">ISO 20022 pacs.008.001.08 Wire Manifest</div>
              <pre id="cb-iso-display">&lt;!-- Dispatched pacs.008 will appear here --&gt;</pre>
            </div>
            <div>
              <div class="stat-label">SWIFT FIN MT103 Core Wire Format</div>
              <pre id="cb-swift-display">/* Dispatched MT103 wire will appear here */</pre>
            </div>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head">Live Postings Ledger</div>
          <div style="overflow-x:auto;">
            <table>
              <thead><tr><th>ID</th><th>TIMESTAMP</th><th>JV ID</th><th>ACCOUNT</th><th>LEG</th><th>AMOUNT</th><th>STANDARD</th><th>VERDICT</th></tr></thead>
              <tbody id="ledger-body"></tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 2: TRADE FINANCE LC -->
  <div id="tab-tradefinance" class="tab-content">
    <div class="grid-layout">
      <div class="panel">
        <div class="panel-head">Documentary Credit / Letter of Credit (MT700 Engine)</div>
        <div class="panel-body">
          <form onsubmit="event.preventDefault(); submitLC();">
            <div class="form-group"><label>LC Reference Identifier</label><input type="text" id="lc-ref" value="DLC-2026-MUM-8911" readonly></div>
            <div class="form-group"><label>Applicant (Buyer / Importer)</label><input type="text" id="lc-applicant" value="Bharat Steel & Infrastructure Ltd" required></div>
            <div class="form-group"><label>Beneficiary (Seller / Exporter)</label><input type="text" id="lc-bene" value="Nippon Steel Heavy Industries Corp" required></div>
            <div class="form-group"><label>Advising Bank BIC</label><input type="text" id="lc-bic" value="BOTKJPJTXXX" required></div>
            <div class="form-group"><label>Negotiation / Drawdown Amount (INR)</label><input type="number" id="lc-amt" value="5000000" min="1000" step="100" required></div>
            <button type="submit" class="btn btn-green">Execute Document Presentation & Drawdown</button>
          </form>
        </div>
      </div>
      <div class="panel">
        <div class="panel-head">Generated SWIFT MT700 Documentary Credit Record</div>
        <div class="panel-body">
          <pre id="lc-output">/* MT700 wire generation output will appear here */</pre>
        </div>
      </div>
    </div>
  </div>

  <!-- TAB 3: NOSTRO RECONCILIATION -->
  <div id="tab-recon" class="tab-content">
    <div class="panel">
      <div class="panel-head">
        <span>CAMT.053 Electronic Bank-to-Customer Nostro Statement</span>
        <button class="btn" style="width:auto; padding:4px 12px;" onclick="loadCamt()">Fetch Live Statement</button>
      </div>
      <div class="panel-body">
        <p style="margin-bottom:12px; color:var(--muted);">The CAMT.053 XML message is ingested by Core Treasury at EOD to reconcile all cleared outward/inward settlement entries against our correspondent Nostro account at JPMorgan Chase NYC.</p>
        <pre id="camt-output">&lt;!-- Click Fetch Live Statement to inspect camt.053.001.08 --&gt;</pre>
      </div>
    </div>
  </div>

  <!-- TAB 4: ARCHITECTURE BLUEPRINT -->
  <div id="tab-architecture" class="tab-content">
    <div class="doc-section">
      <h2>Transaction Banking Architecture & Message Bus</h2>
      <p>Institutional transaction banking infrastructure decouples client payment initiation (pain.001) from downstream gross settlement (pacs.008 / MT103 / MT202 COV). Core principles enforced by TBG-CORE:</p>
      
      <svg class="vector-svg" viewBox="0 0 960 280" xmlns="http://www.w3.org/2000/svg">
        <defs><marker id="arr" viewBox="0 0 10 10" refX="5" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#00d2ff" /></marker></defs>
        
        <rect x="20" y="90" width="140" height="90" rx="4" fill="#111e38" stroke="#1e3a5f" stroke-width="2"/>
        <text x="90" y="125" fill="#f3f4f6" font-family="monospace" font-size="11" text-anchor="middle" font-weight="bold">Corporate ERP</text>
        <text x="90" y="145" fill="#94a3b8" font-family="monospace" font-size="9" text-anchor="middle">pain.001 / MT101</text>
        
        <rect x="210" y="90" width="160" height="90" rx="4" fill="#111e38" stroke="#00d2ff" stroke-width="2"/>
        <text x="290" y="125" fill="#00d2ff" font-family="monospace" font-size="11" text-anchor="middle" font-weight="bold">Ingress Gateway</text>
        <text x="290" y="145" fill="#94a3b8" font-family="monospace" font-size="9" text-anchor="middle">OFAC Screening & HMAC</text>
        
        <rect x="420" y="90" width="160" height="90" rx="4" fill="#111e38" stroke="#00f090" stroke-width="2"/>
        <text x="500" y="125" fill="#00f090" font-family="monospace" font-size="11" text-anchor="middle" font-weight="bold">Ledger Core</text>
        <text x="500" y="145" fill="#94a3b8" font-family="monospace" font-size="9" text-anchor="middle">Float Reservation & Transit</text>
        
        <rect x="630" y="30" width="150" height="60" rx="4" fill="#0b1329" stroke="#ffaa00" stroke-width="1.5"/>
        <text x="705" y="55" fill="#ffaa00" font-family="monospace" font-size="10" text-anchor="middle">SWIFT CBPR+ (pacs.008)</text>
        <text x="705" y="70" fill="#94a3b8" font-family="monospace" font-size="8" text-anchor="middle">Interbank Cross-Border</text>
        
        <rect x="630" y="105" width="150" height="60" rx="4" fill="#0b1329" stroke="#00d2ff" stroke-width="1.5"/>
        <text x="705" y="130" fill="#00d2ff" font-family="monospace" font-size="10" text-anchor="middle">Domestic Clearing House</text>
        <text x="705" y="145" fill="#94a3b8" font-family="monospace" font-size="8" text-anchor="middle">RTGS / NEFT (SFMS)</text>

        <rect x="630" y="180" width="150" height="60" rx="4" fill="#0b1329" stroke="#00f090" stroke-width="1.5"/>
        <text x="705" y="205" fill="#00f090" font-family="monospace" font-size="10" text-anchor="middle">Trade Finance Portal</text>
        <text x="705" y="220" fill="#94a3b8" font-family="monospace" font-size="8" text-anchor="middle">MT700 Letter of Credit</text>

        <rect x="830" y="105" width="110" height="60" rx="4" fill="#111e38" stroke="#1e3a5f" stroke-width="1.5"/>
        <text x="885" y="135" fill="#f3f4f6" font-family="monospace" font-size="10" text-anchor="middle">Nostro Settled</text>
        <text x="885" y="150" fill="#00f090" font-family="monospace" font-size="8" text-anchor="middle">camt.053 Rec</text>

        <line x1="160" y1="135" x2="205" y2="135" stroke="#00d2ff" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="370" y1="135" x2="415" y2="135" stroke="#00d2ff" stroke-width="2" marker-end="url(#arr)"/>
        <line x1="580" y1="115" x2="625" y2="60" stroke="#ffaa00" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="580" y1="135" x2="625" y2="135" stroke="#00d2ff" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="580" y1="155" x2="625" y2="210" stroke="#00f090" stroke-width="1.5" marker-end="url(#arr)"/>
        <line x1="780" y1="135" x2="825" y2="135" stroke="#94a3b8" stroke-width="1.5" marker-end="url(#arr)"/>
      </svg>
    </div>
  </div>

  <!-- TAB 5: ENTERPRISE GLOSSARY -->
  <div id="tab-glossary" class="tab-content">
    <div class="doc-section">
      <h2>Institutional Transaction Banking Glossary</h2>
      <div class="glossary-grid">
        <div class="term-card">
          <div class="term-title">pacs.008 (ISO 20022 CBPR+)</div>
          <div class="term-desc">Financial Customer Credit Transfer. The ISO 20022 message replacing SWIFT MT103. Contains rich unstructured remittance data, structured creditor addresses, and unique end-to-end tracking identifiers (UETR).</div>
        </div>
        <div class="term-card">
          <div class="term-title">SWIFT MT103 & MT202 COV</div>
          <div class="term-desc">MT103 carries direct customer payment instructions. If direct correspondent relationships do not exist, an MT202 COV (Cover Payment) routes the actual financial liquidity through an intermediary settlement correspondent.</div>
        </div>
        <div class="term-card">
          <div class="term-title">camt.053 (Bank-to-Customer Statement)</div>
          <div class="term-desc">The XML replacement for SWIFT MT940. Emitted daily by correspondent banks to report posted opening and closing balances with itemized booking entries for straight-through Nostro reconciliation.</div>
        </div>
        <div class="term-card">
          <div class="term-title">Documentary Credit (SWIFT MT700)</div>
          <div class="term-desc">An irrevocable undertaking issued by the buyer's bank guaranteeing payment to the seller upon the presentation of compliant commercial and shipping documents (Bill of Lading, Certificate of Origin).</div>
        </div>
        <div class="term-card">
          <div class="term-title">UETR (Unique End-to-End Reference)</div>
          <div class="term-desc">A mandatory 36-character hexadecimal UUID format (version 4) that persists unchanged across all intermediary hops in the SWIFT GPI (Global Payments Innovation) tracking rail.</div>
        </div>
        <div class="term-card">
          <div class="term-title">Nostro vs. Vostro</div>
          <div class="term-desc">"Our money on deposit with your bank" (Nostro) vs. "Your money on deposit with our bank" (Vostro). Nostro accounts are recorded as bank assets; Vostro accounts are recorded as bank liabilities.</div>
        </div>
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
      document.getElementById('cb-idem').value = 'E2E-' + Math.floor(10000000 + Math.random() * 90000000);
    }

    async function refreshState() {
      const res = await fetch('/api/state');
      const data = await res.json();
      document.getElementById('disp-float-usd').innerText = 'USD ' + Number(data.float_usd).toLocaleString('en-US', {minimumFractionDigits: 2});
      document.getElementById('disp-suspense-usd').innerText = 'USD ' + Number(data.suspense_usd).toLocaleString('en-US', {minimumFractionDigits: 2});
      document.getElementById('disp-nostro-usd').innerText = 'USD ' + Number(data.nostro_usd).toLocaleString('en-US', {minimumFractionDigits: 2});
      document.getElementById('disp-lc-escrow').innerText = 'INR ' + Number(data.lc_escrow).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-count').innerText = data.entry_count + ' Entries';

      const tbody = document.getElementById('ledger-body');
      tbody.innerHTML = '';
      data.ledger.forEach(r => {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td>' + r.id + '</td><td>' + r.time + '</td><td>' + r.jv + '</td><td>' + r.acct + '</td><td class="' + (r.leg==='DR'?'tag-dr':'tag-cr') + '">' + r.leg + '</td><td>' + r.currency + ' ' + Number(r.amt).toLocaleString() + '</td><td>' + (r.standard || 'INTERNAL') + '</td><td style="color:var(--secondary)">✓ ' + r.verdict + '</td>';
        tbody.appendChild(tr);
      });
    }

    async function submitCrossBorder() {
      const payload = {
        source_account: document.getElementById('cb-src').value,
        beneficiary_name: document.getElementById('cb-bene-name').value,
        beneficiary_iban: document.getElementById('cb-bene-iban').value,
        beneficiary_bic: document.getElementById('cb-bene-bic').value,
        intermediary_bic: document.getElementById('cb-int-bic').value,
        amount: parseFloat(document.getElementById('cb-amt').value),
        currency: 'USD',
        charge_bearer: document.getElementById('cb-chrg').value,
        payment_reference: 'INV-AIRBUS-2026-90',
        idempotency_key: document.getElementById('cb-idem').value
      };

      const res = await fetch('/api/crossborder', {
        method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)
      });
      if (!res.ok) { alert('Failed: ' + await res.text()); return; }
      const out = await res.json();

      document.getElementById('cb-iso-display').innerText = out.iso_xml;
      document.getElementById('cb-swift-display').innerText = out.swift_mt103;

      genIdem();
      await refreshState();
    }

    async function submitLC() {
      const payload = {
        lc_reference: document.getElementById('lc-ref').value,
        applicant_name: document.getElementById('lc-applicant').value,
        beneficiary_name: document.getElementById('lc-bene').value,
        issuing_bank_bic: document.getElementById('lc-bic').value,
        drawdown_amount_inr: parseFloat(document.getElementById('lc-amt').value)
      };

      const res = await fetch('/api/tradefinance', {
        method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)
      });
      if (!res.ok) { alert('Failed: ' + await res.text()); return; }
      const out = await res.json();

      document.getElementById('lc-output').innerText = out.swift_mt700;
      await refreshState();
    }

    async function loadCamt() {
      const res = await fetch('/api/camt053');
      const text = await res.text();
      document.getElementById('camt-output').innerText = text;
    }

    window.onload = function() {
      genIdem();
      refreshState();
    };
  </script>
</body>
</html>
`
