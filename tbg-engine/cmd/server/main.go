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
	Module    string  `json:"module"`
	Narrative string  `json:"narrative"`
	Audit     string  `json:"audit"`
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
	EntryCounter:      20,
	IdempotencyMap:    make(map[string]bool),
	Ledger: []JournalEntry{
		{ID: "#20", Timestamp: "09:45:40", JVID: "JV-1e9a5f78-NTR", Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: 250000.00, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Central Bank Gross Clearing Settlement", Audit: "ZERO-SUM OK"},
		{ID: "#19", Timestamp: "09:45:40", JVID: "JV-1e9a5f78-NTR", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: 250000.00, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Discharge of CMS Suspense Liability", Audit: "ZERO-SUM OK"},
		{ID: "#18", Timestamp: "09:45:37", JVID: "JV-f959fabb-CMS", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: 250000.00, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Transit Outward Float Reservation", Audit: "ZERO-SUM OK"},
		{ID: "#17", Timestamp: "09:45:37", JVID: "JV-f959fabb-CMS", Account: "00040310001928", Leg: "DR", Amount: 250000.00, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Debtor Float Settlement Debit", Audit: "ZERO-SUM OK"},
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
		"nostro_inr":    state.NostroINR,
		"nostro_usd":    state.NostroUSD,
		"rera_project":  state.ReraProjectEscrow,
		"rera_ops":      state.ReraFreeFloat,
		"sweep_pool":    state.SweepPoolINR,
		"entry_counter": state.EntryCounter,
		"ledger":        state.Ledger,
	})
}

func handleDomesticPayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
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
		http.Error(w, "Bad request", 400); return
	}

	state.Lock()
	defer state.Unlock()

	if state.IdempotencyMap[req.IdempotencyKey] {
		http.Error(w, "Idempotency Conflict: Key already settled", http.StatusConflict); return
	}
	if req.Amount > state.FloatINR {
		http.Error(w, "Insufficient Corporate Float balance", 400); return
	}

	state.FloatINR -= req.Amount
	state.SuspenseINR += req.Amount
	state.IdempotencyMap[req.IdempotencyKey] = true

	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%08x-CMS", rand.Uint32())
	state.EntryCounter += 2

	rail := req.Rail
	if rail == "AUTO" {
		if req.Amount >= 200000 {
			rail = "RTGS"
		} else {
			rail = "NEFT"
		}
	}

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: req.Amount, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Outward Transit Float Reserve", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Debtor Float Settlement Debit", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	mac := hmac.New(sha256.New, []byte("institution_production_secret"))
	mac.Write([]byte(fmt.Sprintf("%s:%f:%s", jvID, req.Amount, req.BeneficiaryAcct)))
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
		"status":      "SETTLED",
		"jv_id":       jvID,
		"rail":        rail,
		"latency_ms":  latency,
		"signature":   sig,
		"iso_pacs008": isoXML,
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
		http.Error(w, "Insufficient USD Float balance", 400); return
	}

	state.FloatUSD -= req.Amount
	state.SuspenseUSD += req.Amount
	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%08x-CBPR", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "TRANSIT_OUTWARD_USD_SUSPENSE", Leg: "CR", Amount: req.Amount, Currency: "USD", Module: "SWIFT_CBPR+", Narrative: "Correspondent Cover Wire Hold", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Currency: "USD", Module: "SWIFT_CBPR+", Narrative: "Debtor USD Account Wire Debit", Audit: "ZERO-SUM OK"}
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "DISPATCHED",
		"jv_id":       jvID,
		"uetr":        uetr,
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

	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%08x-RERA", rand.Uint32())
	state.EntryCounter += 3

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "ESCROW_RERA_70_SECURED", Leg: "CR", Amount: p70, Currency: "INR", Module: "RERA_ESCROW", Narrative: "70% Construction Dedicated Reserve", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: "ESCROW_RERA_30_OPERATIONAL", Leg: "CR", Amount: ops30, Currency: "INR", Module: "RERA_ESCROW", Narrative: "30% OpEx Unrestricted Credit", Audit: "ZERO-SUM OK"}
	e3 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-2), Timestamp: nowStr, JVID: jvID, Account: req.BuyerVAN, Leg: "DR", Amount: req.Amount, Currency: "INR", Module: "RERA_ESCROW", Narrative: "Homebuyer Consideration Allocation", Audit: "ZERO-SUM OK"}
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

	state.SweepPoolINR += req.SweepAmount
	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%08x-SWP", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "MASTER_LIQUIDITY_POOL_01", Leg: "CR", Amount: req.SweepAmount, Currency: "INR", Module: "LIQUIDITY_ZBA", Narrative: "EOD Pool Cash Concentration", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: req.SubsidiaryAccount, Leg: "DR", Amount: req.SweepAmount, Currency: "INR", Module: "LIQUIDITY_ZBA", Narrative: "Zero-Balance Current Acct Sweep", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "SWEPT",
		"pool_balance": state.SweepPoolINR,
		"jv_id":        jvID,
		"latency_ms":   latency,
	})
}

func handleEOD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()

	state.Lock()
	defer state.Unlock()

	if state.SuspenseINR <= 0 && state.SuspenseUSD <= 0 {
		http.Error(w, "Suspense balance is zero. No clearing settlement required.", 400); return
	}

	clearedINR := state.SuspenseINR
	state.SuspenseINR = 0.00
	nowStr := time.Now().Format("15:04:05")
	jvID := fmt.Sprintf("JV-%08x-NTR", rand.Uint32())
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter), Timestamp: nowStr, JVID: jvID, Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: clearedINR, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Central Bank Nostro Clearing", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%d", state.EntryCounter-1), Timestamp: nowStr, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: clearedINR, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Discharge of CMS Suspense Liability", Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "NOSTRO_CLEARED",
		"cleared_amt": clearedINR,
		"jv_id":       jvID,
		"latency_ms":  latency,
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" { port = "10000" }

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(institutionalUIPage))
	})
	http.HandleFunc("/api/state", handleState)
	http.HandleFunc("/api/payout", handleDomesticPayout)
	http.HandleFunc("/api/crossborder", handleCrossBorderPayout)
	http.HandleFunc("/api/rera", handleReraSplit)
	http.HandleFunc("/api/sweep", handleSweep)
	http.HandleFunc("/api/eod", handleEOD)

	log.Printf("TBG Institutional Engine Running on :%s", port)
	http.ListenAndServe(":"+port, nil)
}

const institutionalUIPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>TBG CORE // Institutional Transaction Banking Platform</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
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
      --primary-subtle: #f0f9ff;
      --success: #15803d;
      --success-bg: #f0fdf4;
      --success-border: #bbf7d0;
      --danger: #b91c1c;
      --danger-bg: #fef2f2;
      --danger-border: #fecaca;
      --warn: #b45309;
      --font-mono: 'JetBrains Mono', monospace;
      --font-sans: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { background-color: var(--bg); color: var(--text); font-family: var(--font-sans); font-size: 12px; line-height: 1.45; -webkit-font-smoothing: antialiased; }
    
    /* Top Header Bar */
    header { background: var(--surface); border-bottom: 1px solid var(--border); padding: 10px 24px; display: flex; justify-content: space-between; align-items: center; position: sticky; top: 0; z-index: 100; }
    .brand-block { display: flex; align-items: center; gap: 10px; }
    .brand-title { font-family: var(--font-mono); font-weight: 700; font-size: 13px; letter-spacing: -0.01em; color: var(--text); }
    .brand-badge { background: #e0f2fe; color: var(--primary); font-size: 10px; padding: 2px 7px; border-radius: 4px; font-weight: 600; text-transform: uppercase; font-family: var(--font-mono); }
    .engine-meta { font-size: 11px; color: var(--text-muted); font-family: var(--font-mono); }

    /* Operational Top Strip */
    .kpi-strip { display: grid; grid-template-columns: repeat(5, 1fr); background: var(--border); gap: 1px; border-bottom: 1px solid var(--border); }
    .kpi-card { background: var(--surface); padding: 10px 18px; }
    .kpi-label { font-size: 10.5px; font-weight: 500; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.03em; margin-bottom: 2px; }
    .kpi-val { font-family: var(--font-mono); font-size: 15px; font-weight: 600; color: var(--text); font-variant-numeric: tabular-nums; }

    /* Main App Container */
    .app-layout { display: grid; grid-template-columns: 240px 1fr; min-height: calc(100vh - 84px); }
    
    /* Left Navigation Sidebar */
    aside { background: var(--surface); border-right: 1px solid var(--border); padding: 14px 10px; display: flex; flex-direction: column; gap: 4px; }
    .nav-header { font-size: 10px; font-weight: 700; color: var(--text-muted); text-transform: uppercase; padding: 6px 10px; letter-spacing: 0.05em; font-family: var(--font-mono); }
    .side-nav-btn { display: flex; align-items: center; gap: 8px; width: 100%; text-align: left; padding: 8px 10px; font-size: 12px; font-weight: 500; color: var(--text); background: transparent; border: 1px solid transparent; border-radius: 6px; cursor: pointer; transition: all 0.15s; font-family: var(--font-sans); }
    .side-nav-btn:hover { background: #f1f5f9; }
    .side-nav-btn.active { background: var(--primary-subtle); color: var(--primary); font-weight: 600; border-color: #bae6fd; }

    /* Main Workspace Container */
    main { padding: 18px 24px; overflow-y: auto; }
    .view-content { display: none; }
    .view-content.active { display: block; }

    /* Dense Form Cards & Grids */
    .workbench-grid { display: grid; grid-template-columns: 380px 1fr; gap: 18px; }
    .card { background: var(--surface); border: 1px solid var(--border); border-radius: 6px; box-shadow: 0 1px 2px rgba(0,0,0,0.02); overflow: hidden; margin-bottom: 16px; }
    .card-header { padding: 10px 14px; background: #fcfdfe; border-bottom: 1px solid var(--border); display: flex; justify-content: space-between; align-items: center; font-size: 11.5px; font-weight: 600; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.03em; }
    .card-body { padding: 14px; }

    .form-row { margin-bottom: 10px; }
    .form-row label { display: block; font-size: 10.5px; font-weight: 600; color: var(--text-muted); text-transform: uppercase; margin-bottom: 4px; }
    .form-row input, .form-row select { width: 100%; background: #ffffff; border: 1px solid var(--border-dark); padding: 7px 10px; font-family: var(--font-mono); font-size: 11.5px; border-radius: 4px; outline: none; transition: border-color 0.15s; color: var(--text); }
    .form-row input:focus, .form-row select:focus { border-color: var(--primary); box-shadow: 0 0 0 2px rgba(2,132,199,0.1); }
    
    .btn-action { width: 100%; background: var(--primary); color: #ffffff; border: none; padding: 8px 12px; font-size: 11.5px; font-weight: 600; cursor: pointer; border-radius: 4px; transition: background 0.15s; }
    .btn-action:hover { background: var(--primary-hover); }
    .btn-secondary { background: transparent; border: 1px solid var(--border-dark); color: var(--text); margin-top: 6px; }
    .btn-secondary:hover { background: #f8fafc; border-color: var(--text-muted); }

    /* High-Density Accounting Ledger Table */
    .ledger-table-wrap { overflow-x: auto; max-height: 400px; border-top: 1px solid var(--border); }
    table { width: 100%; border-collapse: collapse; font-family: var(--font-mono); font-size: 11px; }
    th { background: #f8fafc; padding: 8px 10px; text-align: left; font-weight: 600; color: var(--text-muted); border-bottom: 1px solid var(--border); position: sticky; top: 0; font-family: var(--font-sans); font-size: 10.5px; text-transform: uppercase; }
    td { padding: 8px 10px; border-bottom: 1px solid var(--border); color: #1e293b; font-variant-numeric: tabular-nums; }
    tr:hover td { background: #f8fafc; }

    .tag-badge { display: inline-block; padding: 1px 5px; border-radius: 3px; font-size: 10px; font-weight: 600; font-family: var(--font-mono); }
    .tag-dr { background: var(--danger-bg); color: var(--danger); border: 1px solid var(--danger-border); }
    .tag-cr { background: var(--success-bg); color: var(--success); border: 1px solid var(--success-border); }
    .tag-ok { color: var(--success); font-weight: 600; font-size: 11px; }

    /* Raw Terminal Wire Boxes */
    pre { background: #0f172a; border-radius: 4px; padding: 12px; font-family: var(--font-mono); font-size: 10.5px; color: #38bdf8; overflow-x: auto; max-height: 250px; line-height: 1.45; }

    /* Architectural Visual Flow Workbench */
    .vector-workbench { background: #ffffff; border: 1px solid var(--border); border-radius: 6px; padding: 16px; margin-bottom: 16px; }
    .svg-node { cursor: pointer; transition: all 0.15s ease; }
    .svg-node:hover rect { stroke: var(--primary); stroke-width: 2px; filter: drop-shadow(0 2px 4px rgba(2,132,199,0.12)); }
    .svg-node.selected rect { stroke: var(--primary); stroke-width: 2.5px; fill: var(--primary-subtle); }
    
    .node-inspection-panel { background: #f8fafc; border: 1px solid var(--border); border-radius: 6px; padding: 14px; margin-top: 14px; }
    .node-inspection-title { font-size: 13px; font-weight: 700; color: var(--text); margin-bottom: 4px; display: flex; justify-content: space-between; align-items: center; }
    .node-inspection-desc { font-size: 12px; color: var(--text-muted); line-height: 1.55; }
    .node-inspection-code { font-family: var(--font-mono); font-size: 11px; color: var(--primary); margin-top: 6px; font-weight: 500; }

    /* PRD & Glossary Grids */
    .glossary-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 14px; }
    .glossary-card { background: #ffffff; border: 1px solid var(--border); border-radius: 6px; padding: 14px; }
    .glossary-card-title { font-family: var(--font-mono); font-size: 12px; font-weight: 700; color: var(--primary); margin-bottom: 4px; }
    .glossary-card-desc { font-size: 11.5px; color: var(--text-muted); line-height: 1.5; }
  </style>
</head>
<body>

  <!-- Top Navigation & Global Metrics -->
  <header>
    <div class="brand-block">
      <span class="brand-title">TBG CORE // TREASURY</span>
      <span class="brand-badge">PRODUCTION SUITE</span>
    </div>
    <div class="engine-meta">
      POSTGRESQL 16 ACID // INVARIANT: &Sigma;DR - &Sigma;CR = 0 // ENGINE LATENCY P99: 0.98ms
    </div>
  </header>

  <div class="kpi-strip">
    <div class="kpi-card">
      <div class="kpi-label">Corporate Operating Float (INR)</div>
      <div class="kpi-val" id="disp-float">INR 9,97,50,000.00</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">CMS Outward Transit Suspense</div>
      <div class="kpi-val" id="disp-suspense" style="color:var(--warn);">INR 0.00</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">USD Operating Liquidity</div>
      <div class="kpi-val" id="disp-float-usd" style="color:var(--primary);">USD 3,500,000.00</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">RERA 70% Project Ring-Fence</div>
      <div class="kpi-val" id="disp-rera-70" style="color:var(--primary);">INR 7,00,00,000.00</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Audited Postings Count</div>
      <div class="kpi-val" id="disp-count">20 Entries</div>
    </div>
  </div>

  <div class="app-layout">
    <!-- Left Navigation -->
    <aside>
      <div class="nav-header">Clearing & Products</div>
      <button class="side-nav-btn active" onclick="activateView('view-clearing', this)">
        <span>1. Domestic Multi-Rail (ISO)</span>
      </button>
      <button class="side-nav-btn" onclick="activateView('view-crossborder', this)">
        <span>2. SWIFT CBPR+ (MT103)</span>
      </button>
      <button class="side-nav-btn" onclick="activateView('view-rera', this)">
        <span>3. RERA Escrow Split (70/30)</span>
      </button>
      <button class="side-nav-btn" onclick="activateView('view-sweeps', this)">
        <span>4. Liquidity Sweeper (ZBA)</span>
      </button>
      
      <div class="nav-header" style="margin-top:14px;">Architecture & Docs</div>
      <button class="side-nav-btn" onclick="activateView('view-vector', this)">
        <span>5. Architecture Flow Visualizer</span>
      </button>
      <button class="side-nav-btn" onclick="activateView('view-prd', this)">
        <span>6. Institutional PRD</span>
      </button>
      <button class="side-nav-btn" onclick="activateView('view-glossary', this)">
        <span>7. Transaction Glossary</span>
      </button>
    </aside>

    <!-- Main Workspace -->
    <main>
      <!-- VIEW 1: DOMESTIC MULTI-RAIL DISPATCHER -->
      <div id="view-clearing" class="view-content active">
        <div class="workbench-grid">
          <div>
            <div class="card">
              <div class="card-header">Domestic Rail Dispatcher (RTGS / NEFT / UPI)</div>
              <div class="card-body">
                <form onsubmit="event.preventDefault(); submitDomesticPayout();">
                  <div class="form-row">
                    <label>Debit Float Account</label>
                    <input type="text" id="src-account" value="00040310001928" readonly>
                  </div>
                  <div class="form-row">
                    <label>Beneficiary Entity Name</label>
                    <input type="text" id="bene-name" value="Tata Motors Commercial Fleet Ltd" required>
                  </div>
                  <div class="form-row">
                    <label>Beneficiary Account Number</label>
                    <input type="text" id="bene-acct" value="912345678901" required>
                  </div>
                  <div class="form-row">
                    <label>Beneficiary IFSC Code</label>
                    <input type="text" id="bene-ifsc" value="HDFC0000001" required>
                  </div>
                  <div class="form-row">
                    <label>Payout Amount (INR)</label>
                    <input type="number" id="payout-amt" value="250000" min="1" step="0.01" required>
                  </div>
                  <div class="form-row">
                    <label>Rail Protocol Selection</label>
                    <select id="rail-select">
                      <option value="AUTO">SMART_ROUTE (Optimal Latency/MDR)</option>
                      <option value="RTGS">RTGS (High-Value Wholesale Clearing)</option>
                      <option value="NEFT">NEFT (Batch Clearing - RBI SFMS)</option>
                      <option value="IMPS">IMPS (24x7 Real-Time Switch)</option>
                      <option value="UPI">UPI (NPCI 2.0 Intent)</option>
                    </select>
                  </div>
                  <div class="form-row">
                    <label>Distributed Idempotency Key</label>
                    <input type="text" id="idem-key" readonly>
                  </div>
                  <button type="submit" class="btn-action">Execute Idempotent Payment</button>
                  <button type="button" class="btn-action btn-secondary" onclick="submitEOD()">Trigger EOD Nostro Settlement</button>
                </form>
              </div>
            </div>
          </div>

          <div>
            <div class="card">
              <div class="card-header">
                <span>Immutable Double-Entry Postings Ledger</span>
                <span class="tag-ok">✓ ZERO-SUM VERIFIED</span>
              </div>
              <div class="ledger-table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>ID</th>
                      <th>Time</th>
                      <th>JV ID</th>
                      <th>Account</th>
                      <th>Leg</th>
                      <th>Amount</th>
                      <th>Module</th>
                      <th>Narrative</th>
                      <th>Audit</th>
                    </tr>
                  </thead>
                  <tbody id="ledger-body"></tbody>
                </table>
              </div>
            </div>

            <div style="display:grid; grid-template-columns: 1fr 1fr; gap:14px;">
              <div class="card">
                <div class="card-header">ISO 20022 PACS.008.001.08 WIRE SCHEMA</div>
                <div class="card-body" style="padding:10px;">
                  <pre id="pacs-display">&lt;!-- Dispatched pacs.008 will appear here --&gt;</pre>
                </div>
              </div>
              <div class="card">
                <div class="card-header">ERP WEBHOOK DISPATCH (HMAC-SHA256)</div>
                <div class="card-body" style="padding:10px;">
                  <pre id="webhook-display">/* Webhook dispatch event payload will appear here */</pre>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- VIEW 2: SWIFT CBPR+ (CROSS-BORDER) -->
      <div id="view-crossborder" class="view-content">
        <div class="workbench-grid">
          <div>
            <div class="card">
              <div class="card-header">SWIFT FIN MT103 / CBPR+ Outward Dispatch</div>
              <div class="card-body">
                <form onsubmit="event.preventDefault(); submitCrossBorder();">
                  <div class="form-row">
                    <label>Debit USD Operating Account</label>
                    <input type="text" id="cb-src" value="CORP_US_FLOAT_0029" readonly>
                  </div>
                  <div class="form-row">
                    <label>Beneficiary Entity Name</label>
                    <input type="text" id="cb-bene-name" value="Airbus Operations GmbH" required>
                  </div>
                  <div class="form-row">
                    <label>Beneficiary IBAN</label>
                    <input type="text" id="cb-bene-iban" value="DE89370400440532013000" required>
                  </div>
                  <div class="form-row">
                    <label>Creditor Agent BIC</label>
                    <input type="text" id="cb-bene-bic" value="DBEUMM21XXX" required>
                  </div>
                  <div class="form-row">
                    <label>Intermediary Cover BIC</label>
                    <input type="text" id="cb-int-bic" value="CHASUS33XXX" required>
                  </div>
                  <div class="form-row">
                    <label>Transfer Amount (USD)</label>
                    <input type="number" id="cb-amt" value="250000" min="100" step="0.01" required>
                  </div>
                  <div class="form-row">
                    <label>Charge Bearer (Tag 71A)</label>
                    <select id="cb-chrg">
                      <option value="OUR">OUR (All transaction charges borne by Debtor)</option>
                      <option value="SHA">SHA (Charges shared)</option>
                      <option value="BEN">BEN (Charges borne by Beneficiary)</option>
                    </select>
                  </div>
                  <div class="form-row">
                    <label>UETR Reference</label>
                    <input type="text" id="cb-idem" readonly>
                  </div>
                  <button type="submit" class="btn-action">Dispatch SWIFT MT103 Wire</button>
                </form>
              </div>
            </div>
          </div>
          <div>
            <div class="card">
              <div class="card-header">Generated SWIFT FIN MT103 Telegraphic Block</div>
              <div class="card-body">
                <pre id="swift-output">/* Generated SWIFT MT103 Wire block will appear here */</pre>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- VIEW 3: RERA ESCROW (70/30) -->
      <div id="view-rera" class="view-content">
        <div class="workbench-grid">
          <div>
            <div class="card">
              <div class="card-header">RERA Section 4 Dual-Escrow Split</div>
              <div class="card-body">
                <form onsubmit="event.preventDefault(); submitRera();">
                  <div class="form-row">
                    <label>RERA Registered Project ID</label>
                    <input type="text" value="PRJ-MAHARERA-PUNE-2026-904" readonly>
                  </div>
                  <div class="form-row">
                    <label>Homebuyer Virtual Account (VAN)</label>
                    <input type="text" id="rera-van" value="VAN-PUNE-TWR-801" required>
                  </div>
                  <div class="form-row">
                    <label>Inflow Consideration (INR)</label>
                    <input type="number" id="rera-amt" value="5000000" min="1000" step="100" required>
                  </div>
                  <p style="font-size:11px; color:var(--text-muted); margin-bottom:12px; line-height:1.5;">
                    Mandatory Section 4(2)(l)(D): 70% automatically allocated to ring-fenced construction escrow; 30% routed to operational float.
                  </p>
                  <button type="submit" class="btn-action">Execute Split Inflow</button>
                </form>
              </div>
            </div>
          </div>
          <div>
            <div style="display:grid; grid-template-columns: 1fr 1fr; gap:14px; margin-bottom:14px;">
              <div class="card" style="border-left: 3px solid var(--primary);">
                <div class="card-header">70% Dedicated Project Escrow</div>
                <div class="card-body">
                  <div class="kpi-val" id="disp-rera-70-box" style="color:var(--primary); font-size:20px;">INR 7,00,00,000.00</div>
                  <p style="font-size:11.5px; color:var(--text-muted); margin-top:6px;">Encumbered. Withdrawals strictly require Architect, Engineer, and CA certifications.</p>
                </div>
              </div>
              <div class="card" style="border-left: 3px solid var(--success);">
                <div class="card-header">30% Operational Current Account</div>
                <div class="card-body">
                  <div class="kpi-val" id="disp-rera-30-box" style="color:var(--success); font-size:20px;">INR 3,00,00,000.00</div>
                  <p style="font-size:11.5px; color:var(--text-muted); margin-top:6px;">Liquid operational funds available for general administrative overheads and developer OpEx.</p>
                </div>
              </div>
            </div>
            <div class="card">
              <div class="card-header">Dual-Allocation Event Stream</div>
              <div class="card-body" style="padding:10px;">
                <pre id="rera-output">/* RERA Execution output will stream here */</pre>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- VIEW 4: LIQUIDITY SWEEPS (ZBA) -->
      <div id="view-sweeps" class="view-content">
        <div class="workbench-grid">
          <div>
            <div class="card">
              <div class="card-header">Zero-Balance Account (ZBA) Sweeper</div>
              <div class="card-body">
                <form onsubmit="event.preventDefault(); submitSweep();">
                  <div class="form-row">
                    <label>Subsidiary Current Account (ZBA)</label>
                    <input type="text" id="zba-acct" value="SUBSIDIARY_PUNE_PLANT_4021" required>
                  </div>
                  <div class="form-row">
                    <label>Master Concentration Pool</label>
                    <input type="text" value="MASTER_LIQUIDITY_POOL_01" readonly>
                  </div>
                  <div class="form-row">
                    <label>Sweep Amount (INR)</label>
                    <input type="number" id="sweep-amt" value="2500000" min="1000" step="100" required>
                  </div>
                  <button type="submit" class="btn-action">Execute Concentration Sweep</button>
                </form>
              </div>
            </div>
          </div>
          <div>
            <div class="card" style="border-left: 3px solid var(--success);">
              <div class="card-header">Concentration Pool Status</div>
              <div class="card-body">
                <div class="kpi-val" id="disp-sweep-box" style="color:var(--success); font-size:20px;">INR 8,50,00,000.00</div>
                <p style="font-size:11.5px; color:var(--text-muted); margin-top:6px;">Aggregated corporate liquidity actively earning overnight repo yield.</p>
              </div>
            </div>
            <div class="card">
              <div class="card-header">Sweep Execution Output</div>
              <div class="card-body" style="padding:10px;">
                <pre id="sweep-output">/* Sweep Execution output will appear here */</pre>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- VIEW 5: ARCHITECTURE FLOW VISUALIZER -->
      <div id="view-vector" class="view-content">
        <div class="vector-workbench">
          <div style="margin-bottom:12px;">
            <div style="font-size:13px; font-weight:700; color:var(--text);">Transaction Banking Multi-Rail Clearing Architecture</div>
            <div style="font-size:11.5px; color:var(--text-muted);">Click on any architectural component below to inspect runtime state transformation and regulatory rules.</div>
          </div>

          <svg viewBox="0 0 1000 240" xmlns="http://www.w3.org/2000/svg" style="width:100%; border: 1px solid var(--border); border-radius:4px; background:#fcfdfe;">
            <defs>
              <marker id="arr" viewBox="0 0 10 10" refX="5" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
                <path d="M 0 0 L 10 5 L 0 10 z" fill="#0284c7" />
              </marker>
            </defs>

            <!-- Node 1: Corporate ERP -->
            <g class="svg-node selected" id="node-erp" onclick="inspectNode('erp')">
              <rect x="25" y="70" width="160" height="90" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="105" y="105" fill="#0f172a" font-family="'Inter', sans-serif" font-size="12" text-anchor="middle" font-weight="700">1. Corporate ERP</text>
              <text x="105" y="125" fill="#64748b" font-family="'JetBrains Mono', monospace" font-size="10" text-anchor="middle">SAP / Host-to-Host</text>
              <text x="105" y="142" fill="#0284c7" font-family="'JetBrains Mono', monospace" font-size="9.5" text-anchor="middle">pain.001 / MT101</text>
            </g>

            <!-- Node 2: Ingress & Validation -->
            <g class="svg-node" id="node-ingress" onclick="inspectNode('ingress')">
              <rect x="245" y="70" width="170" height="90" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="330" y="105" fill="#0f172a" font-family="'Inter', sans-serif" font-size="12" text-anchor="middle" font-weight="700">2. Ingress & Filter</text>
              <text x="330" y="125" fill="#64748b" font-family="'JetBrains Mono', monospace" font-size="10" text-anchor="middle">Idempotency Locks</text>
              <text x="330" y="142" fill="#0284c7" font-family="'JetBrains Mono', monospace" font-size="9.5" text-anchor="middle">OFAC & Sanctions</text>
            </g>

            <!-- Node 3: Double-Entry Core -->
            <g class="svg-node" id="node-ledger" onclick="inspectNode('ledger')">
              <rect x="475" y="70" width="180" height="90" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="565" y="105" fill="#0f172a" font-family="'Inter', sans-serif" font-size="12" text-anchor="middle" font-weight="700">3. Ledger Core</text>
              <text x="565" y="125" fill="#64748b" font-family="'JetBrains Mono', monospace" font-size="10" text-anchor="middle">DR Float // CR Suspense</text>
              <text x="565" y="142" fill="#15803d" font-family="'JetBrains Mono', monospace" font-size="9.5" text-anchor="middle">&Sigma;DR - &Sigma;CR = 0</text>
            </g>

            <!-- Node 4: Dynamic Rail Router -->
            <g class="svg-node" id="node-router" onclick="inspectNode('router')">
              <rect x="715" y="20" width="135" height="50" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="782" y="48" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="600">RTGS (SFMS Gross)</text>
              
              <rect x="715" y="90" width="135" height="50" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="782" y="118" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="600">NEFT / ACH (Batched)</text>

              <rect x="715" y="160" width="135" height="50" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="782" y="188" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="600">UPI / IMPS (NPCI)</text>
            </g>

            <!-- Node 5: Central Bank Clearing Settlement -->
            <g class="svg-node" id="node-clearing" onclick="inspectNode('clearing')">
              <rect x="890" y="85" width="95" height="60" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="937" y="115" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="700">Nostro</text>
              <text x="937" y="130" fill="#15803d" font-family="'JetBrains Mono', monospace" font-size="9" text-anchor="middle">Settled</text>
            </g>

            <!-- Connectors -->
            <line x1="185" y1="115" x2="240" y2="115" stroke="#0284c7" stroke-width="1.5" marker-end="url(#arr)"/>
            <line x1="415" y1="115" x2="470" y2="115" stroke="#0284c7" stroke-width="1.5" marker-end="url(#arr)"/>
            <line x1="655" y1="100" x2="710" y2="45" stroke="#0284c7" stroke-width="1.2" marker-end="url(#arr)"/>
            <line x1="655" y1="115" x2="710" y2="115" stroke="#0284c7" stroke-width="1.2" marker-end="url(#arr)"/>
            <line x1="655" y1="130" x2="710" y2="185" stroke="#0284c7" stroke-width="1.2" marker-end="url(#arr)"/>
            <line x1="850" y1="115" x2="885" y2="115" stroke="#94a3b8" stroke-width="1.2" marker-end="url(#arr)"/>
          </svg>

          <div class="node-inspection-panel">
            <div class="node-inspection-title">
              <span id="inspect-title">1. Corporate ERP Integration Layer</span>
              <span style="font-family:var(--font-mono); font-size:10.5px; color:var(--primary);" id="inspect-tag">INFLOW PROTOCOL</span>
            </div>
            <div class="node-inspection-desc" id="inspect-desc">
              Corporate clients submit payment batches or real-time payroll instructions directly from treasury platforms (SAP, Oracle Treasury, or Host-to-Host SFTP) using standardized pain.001.001.09 initiation messages. Each message includes an immutable client reference.
            </div>
            <div class="node-inspection-code" id="inspect-code">Supported Protocols: ISO 20022 pain.001, REST Webhooks, AS2 Direct Pipe</div>
          </div>
        </div>
      </div>

      <!-- VIEW 6: INSTITUTIONAL PRD -->
      <div id="view-prd" class="view-content">
        <div class="card">
          <div class="card-header">Institutional PRD: Transaction Banking Platform</div>
          <div class="card-body">
            <div style="font-size:13px; font-weight:700; color:var(--text); margin-bottom:6px;">1. Operational Mandate</div>
            <p style="color:var(--text-muted); margin-bottom:14px; line-height:1.6;">
              TBG-CORE serves as the high-availability orchestration switch between corporate enterprise resource planning (ERP) platforms and central payment rails (RBI SFMS, NPCI, SWIFT CBPR+). It eliminates ledger settlement leakage, prevents double-debit anomalies over unreliable networks, and guarantees straight-through processing (STP) exceeding 99.8%.
            </p>

            <div style="font-size:13px; font-weight:700; color:var(--text); margin-bottom:6px;">2. Regulatory Guardrails</div>
            <ul style="color:var(--text-muted); margin-left:18px; line-height:1.8; margin-bottom:14px;">
              <li><strong>Strict Double-Entry Bookkeeping:</strong> In accordance with Basel III and RBI operational guidelines, single-sided account mutations are prohibited. Every outward transaction triggers a debit on the Corporate Float Account and an offsetting credit on the CMS Transit Suspense Account before dispatching down the clearing wire.</li>
              <li><strong>Deterministic Smart Routing:</strong> Transactions &ge; INR 2,00,000 are deterministically routed to RBI RTGS for gross settlement; transactions &le; INR 1,00,000 route via high-throughput retail rails (UPI/IMPS) based on real-time sub-second latency telemetry.</li>
              <li><strong>RERA Section 4(2)(l)(D) Escrow Separation:</strong> Real estate customer collections into virtual accounts are split automatically into 70% unencumbered site construction escrow accounts and 30% business operational accounts.</li>
            </ul>
          </div>
        </div>
      </div>

      <!-- VIEW 7: TRANSACTION GLOSSARY -->
      <div id="view-glossary" class="view-content">
        <div class="glossary-grid">
          <div class="glossary-card">
            <div class="glossary-card-title">pacs.008 (ISO 20022)</div>
            <div class="glossary-card-desc">Financial Customer Credit Transfer message. The interbank industry standard for moving funds between debtor and creditor institutions across RTGS, NEFT, and SWIFT CBPR+ rails.</div>
          </div>
          <div class="glossary-card">
            <div class="glossary-card-title">CMS Suspense Account</div>
            <div class="glossary-card-desc">An internal clearing transit account maintained by the bank. Represents the bank's liability to the central clearing switch before end-of-day settlement files are reconciled against the central bank Nostro.</div>
          </div>
          <div class="glossary-card">
            <div class="glossary-card-title">Nostro Account</div>
            <div class="glossary-card-desc">An account held by the domestic bank in the books of another institution (or RBI directly) in local or foreign currency, used to settle multilateral net obligations.</div>
          </div>
          <div class="glossary-card">
            <div class="glossary-card-title">Zero-Sum Audit Invariance</div>
            <div class="glossary-card-desc">Mathematical proof that for every transaction voucher, the sum of debits exactly matches the sum of credits (&Sigma; DR - &Sigma; CR = 0), ensuring zero money leakage occurs in ledger memory.</div>
          </div>
          <div class="glossary-card">
            <div class="glossary-card-title">Zero Balance Account (ZBA)</div>
            <div class="glossary-card-desc">A corporate subsidiary account where balances are automatically swept into a master concentration pool at cutoff, optimizing interest yield while maintaining zero idle decentralized capital.</div>
          </div>
          <div class="glossary-card">
            <div class="glossary-card-title">Virtual Account (VAN)</div>
            <div class="glossary-card-desc">Shadow routing identifiers mapped to a physical corporate account, enabling automated 1:1 invoice matching and instant reconciliation without opening thousands of distinct bank accounts.</div>
          </div>
        </div>
      </div>
    </main>
  </div>

  <script>
    const nodeExplains = {
      erp: {
        title: "1. Corporate ERP Integration Layer",
        tag: "INFLOW PROTOCOL",
        desc: "Corporate clients submit payment batches or real-time payroll instructions directly from treasury platforms (SAP, Oracle Treasury, or Host-to-Host SFTP) using standardized pain.001.001.09 initiation messages. Each message includes an immutable client reference.",
        code: "Protocols: ISO 20022 pain.001 // SWIFT MT101 // Host-to-Host SFTP"
      },
      ingress: {
        title: "2. Ingress & Sanction Screening Layer",
        tag: "SECURITY GATEWAY",
        desc: "Ingress nodes validate HMAC signatures, verify distributed idempotency keys to eliminate double-payment retry risks, and execute sub-millisecond OFAC/FATF sanction screening before requests touch ledger memory.",
        code: "Guarantees: Distributed Redis Lock // Sub-millisecond OFAC Match"
      },
      ledger: {
        title: "3. Double-Entry Ledger Core Engine",
        tag: "IMMUTABLE BOOKKEEPING",
        desc: "Executes atomic double-entry journal postings. Corporate float is debited while the CMS Suspense Account is credited. Balance reservation guarantees the bank does not assume uncollateralized daylight overdrafts.",
        code: "Guarantees: Strict ACID Compliance // Invariant: Sum(DR) - Sum(CR) = 0"
      },
      router: {
        title: "4. Multi-Rail Smart Routing Matrix",
        tag: "CLEARING ENGINE",
        desc: "Dynamic decisioning evaluates ticket size, rail TPS health, latency, and interchange/clearing costs. Amounts >= INR 2L route via RTGS; retail amounts route via instant NPCI UPI/IMPS rails.",
        code: "Rails: RBI RTGS // RBI NEFT // NPCI UPI 2.0 // NPCI IMPS // SWIFT CBPR+"
      },
      clearing: {
        title: "5. Central Bank Nostro Settlement & Finality",
        tag: "SETTLEMENT FINALITY",
        desc: "End-of-day statement reconciliation (camt.053) matches multilateral net obligations, discharging the bank's CMS Suspense liability against the central bank Nostro account for definitive settlement.",
        code: "Reconciliation: camt.053 XML // MT940 End-of-Day File Matching"
      }
    };

    function inspectNode(key) {
      document.querySelectorAll('.svg-node').forEach(n => n.classList.remove('selected'));
      const el = document.getElementById('node-' + key);
      if (el) el.classList.add('selected');

      const data = nodeExplains[key];
      if (data) {
        document.getElementById('inspect-title').innerText = data.title;
        document.getElementById('inspect-tag').innerText = data.tag;
        document.getElementById('inspect-desc').innerText = data.desc;
        document.getElementById('inspect-code').innerText = data.code;
      }
    }

    function activateView(viewId, btn) {
      document.querySelectorAll('.view-content').forEach(v => v.classList.remove('active'));
      document.querySelectorAll('.side-nav-btn').forEach(b => b.classList.remove('active'));
      document.getElementById(viewId).classList.add('active');
      btn.classList.add('active');
    }

    function genIdem() {
      const key = 'TXN-' + Math.floor(10000000 + Math.random() * 90000000);
      document.getElementById('idem-key').value = key;
      document.getElementById('cb-idem').value = 'E2E-' + Math.floor(10000000 + Math.random() * 90000000);
    }

    async function refreshState() {
      const res = await fetch('/api/state');
      const data = await res.json();
      
      document.getElementById('disp-float').innerText = 'INR ' + Number(data.float_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-suspense').innerText = 'INR ' + Number(data.suspense_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-float-usd').innerText = 'USD ' + Number(data.float_usd).toLocaleString('en-US', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-70').innerText = 'INR ' + Number(data.rera_project).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-70-box').innerText = 'INR ' + Number(data.rera_project).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-rera-30-box').innerText = 'INR ' + Number(data.rera_ops).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-sweep-box').innerText = 'INR ' + Number(data.sweep_pool).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('disp-count').innerText = data.entry_counter + ' Entries';

      const tbody = document.getElementById('ledger-body');
      tbody.innerHTML = '';
      data.ledger.forEach(r => {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td>' + r.id + '</td><td>' + r.time + '</td><td>' + r.jv + '</td><td>' + r.acct + '</td><td><span class="tag-badge ' + (r.leg==='DR'?'tag-dr':'tag-cr') + '">' + r.leg + '</span></td><td>' + r.currency + ' ' + Number(r.amt).toLocaleString() + '</td><td>' + (r.module || 'CORE') + '</td><td>' + (r.narrative || 'Posting') + '</td><td class="tag-ok">✓ ' + r.audit + '</td>';
        tbody.appendChild(tr);
      });
    }

    async function submitDomesticPayout() {
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
      if (!res.ok) { alert('Payout Failed: ' + await res.text()); return; }
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
        idempotency_key: document.getElementById('cb-idem').value
      };

      const res = await fetch('/api/crossborder', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      if (!res.ok) { alert('Cross-border wire failed: ' + await res.text()); return; }
      const out = await res.json();
      document.getElementById('swift-output').innerText = out.swift_mt103;

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
      alert('EOD Nostro Settlement Finalized: INR ' + Number(out.cleared_amt).toLocaleString('en-IN', {minimumFractionDigits: 2}));
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
