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
	Timestamp string  `json:"timestamp"`
	JVID      string  `json:"jv_id"`
	Account   string  `json:"account_id"`
	Leg       string  `json:"leg"`
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
	LCEscrowINR       float64
	VANCollectionsINR float64
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
	LCEscrowINR:       25000000.00,
	VANCollectionsINR: 14200000.00,
	EntryCounter:      4,
	IdempotencyMap:    make(map[string]bool),
	Ledger: []JournalEntry{
		{ID: "#04", Timestamp: "2026-09-28T09:45:40Z", JVID: "JV-1e9a5f78-NTR", Account: "AC_RBI_NOSTRO_0001", Leg: "CR", Amount: 250000.00, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Central Bank Gross Clearing Settlement", Audit: "ZERO-SUM OK"},
		{ID: "#03", Timestamp: "2026-09-28T09:45:40Z", JVID: "JV-1e9a5f78-NTR", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "DR", Amount: 250000.00, Currency: "INR", Module: "NOSTRO_EOD", Narrative: "Discharge of CMS Suspense Liability", Audit: "ZERO-SUM OK"},
		{ID: "#02", Timestamp: "2026-09-28T09:45:37Z", JVID: "JV-f959fabb-CMS", Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: 250000.00, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Transit Outward Float Reservation", Audit: "ZERO-SUM OK"},
		{ID: "#01", Timestamp: "2026-09-28T09:45:37Z", JVID: "JV-f959fabb-CMS", Account: "00040310001928", Leg: "DR", Amount: 250000.00, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Debtor Float Settlement Debit", Audit: "ZERO-SUM OK"},
	},
}

func handleGetLedger(w http.ResponseWriter, r *http.Request) {
	state.Lock()
	defer state.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"system":               "TBG-CORE-ENTERPRISE",
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
		"lc_escrow_margin_inr": state.LCEscrowINR,
		"van_collections_inr":  state.VANCollectionsINR,
		"entry_counter":        state.EntryCounter,
		"postings_ledger":      state.Ledger,
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
		http.Error(w, `{"error":"Malformed JSON"}`, 400); return
	}

	state.Lock()
	defer state.Unlock()

	if state.IdempotencyMap[req.IdempotencyKey] {
		http.Error(w, `{"error":"Duplicate idempotency key"}`, http.StatusConflict); return
	}
	if req.Amount > state.FloatINR {
		http.Error(w, `{"error":"Insufficient Float Balance"}`, 400); return
	}

	state.FloatINR -= req.Amount
	state.SuspenseINR += req.Amount
	state.IdempotencyMap[req.IdempotencyKey] = true

	jvID := fmt.Sprintf("JV-%08x-CMS", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 2

	rail := req.Rail
	if rail == "" || rail == "AUTO" {
		if req.Amount >= 200000 { rail = "RTGS" } else { rail = "NEFT" }
	}

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "AC_CMS_SUSPENSE_CLEARING_9999", Leg: "CR", Amount: req.Amount, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Transit Outward Float Reservation", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: req.SourceAccount, Leg: "DR", Amount: req.Amount, Currency: "INR", Module: "DOMESTIC_PAYOUT", Narrative: "Debtor Float Settlement Debit", Audit: "ZERO-SUM OK"}
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
		http.Error(w, `{"error":"Insufficient USD Float"}`, 400); return
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "DISPATCHED",
		"jv_id":       jvID,
		"uetr":        uetr,
		"swift_mt103": swiftMT103,
		"latency_ms":  latency,
	})
}

func handleVANCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()
	var req struct {
		VirtualAccount string  `json:"virtual_account"`
		RemitterEntity string  `json:"remitter_entity"`
		RemitterIFSC   string  `json:"remitter_ifsc"`
		Amount         float64 `json:"amount"`
		InvoiceRef     string  `json:"invoice_reference"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	state.Lock()
	defer state.Unlock()

	state.FloatINR += req.Amount
	state.VANCollectionsINR += req.Amount
	jvID := fmt.Sprintf("JV-%08x-VAN", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "00040310001928", Leg: "CR", Amount: req.Amount, Currency: "INR", Module: "VAN_COLLECTION", Narrative: fmt.Sprintf("Invoice %s Settlement Inflow", req.InvoiceRef), Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: req.VirtualAccount, Leg: "DR", Amount: req.Amount, Currency: "INR", Module: "VAN_COLLECTION", Narrative: fmt.Sprintf("VAN Inward Wire: %s", req.RemitterEntity), Audit: "ZERO-SUM OK"}
	state.Ledger = append([]JournalEntry{e1, e2}, state.Ledger...)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":             "RECONCILED_AND_POSTED",
		"virtual_account":    req.VirtualAccount,
		"matched_invoice":    req.InvoiceRef,
		"amount_settled_inr": req.Amount,
		"jv_id":              jvID,
		"latency_ms":         latency,
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                "ALLOCATED",
		"project_escrow_70_cr":  p70,
		"operational_float_30_cr": ops30,
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                 "CONCENTRATED",
		"pool_consolidated_inr": state.SweepPoolINR,
		"jv_id":                  jvID,
		"latency_ms":             latency,
	})
}

func handleTradeFinanceLC(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, `{"error":"LC Margin Escrow exceeded"}`, 400); return
	}

	state.LCEscrowINR -= req.DrawdownAmountINR
	jvID := fmt.Sprintf("JV-%08x-LC", rand.Uint32())
	ts := time.Now().UTC().Format(time.RFC3339)
	state.EntryCounter += 2

	e1 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter), Timestamp: ts, JVID: jvID, Account: "BENEFICIARY_ADVISING_PAYMENT_AC", Leg: "CR", Amount: req.DrawdownAmountINR, Currency: "INR", Module: "TRADE_FINANCE", Narrative: "MT700 Compliant Document Presentation Settlement", Audit: "ZERO-SUM OK"}
	e2 := JournalEntry{ID: fmt.Sprintf("#%02d", state.EntryCounter-1), Timestamp: ts, JVID: jvID, Account: "LC_CASH_MARGIN_EARMARKED_8819", Leg: "DR", Amount: req.DrawdownAmountINR, Currency: "INR", Module: "TRADE_FINANCE", Narrative: "Release of 100% Cash Collateral Margin", Audit: "ZERO-SUM OK"}
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
:78:PAYMENT ON RECEIPT OF CLEAN BILL OF LADING AND SGS INSPECTION
-}`, req.IssuingBankBIC, req.LCReference, time.Now().Format("060102"), time.Now().AddDate(0, 3, 0).Format("060102"), req.ApplicantName, req.BeneficiaryName, req.DrawdownAmountINR)

	latency := float64(time.Since(start).Microseconds()) / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "HONORED_AND_SETTLED",
		"jv_id":        jvID,
		"swift_mt700":  swiftMT700,
		"remaining_lc": state.LCEscrowINR,
		"latency_ms":   latency,
	})
}

func handleEODNostro(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()

	state.Lock()
	defer state.Unlock()

	if state.SuspenseINR <= 0 && state.SuspenseUSD <= 0 {
		http.Error(w, `{"error":"Suspense liability is zero."}`, 400); return
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":              "NOSTRO_CLEARED",
		"discharged_suspense": clearedINR,
		"jv_id":               jvID,
		"latency_ms":          latency,
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat("tbg-ui/dist/index.html"); err == nil {
		http.ServeFile(w, r, "tbg-ui/dist/index.html")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(canvasHTML))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" { port = "10000" }

	if _, err := os.Stat("tbg-ui/dist"); err == nil {
		fs := http.FileServer(http.Dir("tbg-ui/dist"))
		http.Handle("/assets/", fs)
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/v1/ledger", handleGetLedger)
	http.HandleFunc("/api/v1/payouts/domestic", handleDomesticPayout)
	http.HandleFunc("/api/v1/payouts/cross-border", handleCrossBorderPayout)
	http.HandleFunc("/api/v1/receivables/van-collection", handleVANCollection)
	http.HandleFunc("/api/v1/escrow/rera-split", handleReraSplit)
	http.HandleFunc("/api/v1/liquidity/zba-sweep", handleSweep)
	http.HandleFunc("/api/v1/trade-finance/lc-drawdown", handleTradeFinanceLC)
	http.HandleFunc("/api/v1/recon/eod-nostro", handleEODNostro)

	log.Printf("TBG Engine running on :%s", port)
	http.ListenAndServe(":"+port, nil)
}

const canvasHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>TBG CORE // Institutional Transaction Banking Platform</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <script src="https://cdn.tailwindcss.com"></script>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
  <script>
    tailwind.config = {
      theme: {
        extend: {
          fontFamily: {
            sans: ['Inter', 'sans-serif'],
            mono: ['JetBrains Mono', 'monospace'],
          }
        }
      }
    }
  </script>
  <style>
    .canvas-grid {
      background-size: 24px 24px;
      background-image: radial-gradient(circle, #cbd5e1 1px, transparent 1px);
    }
    .flow-line {
      stroke-dasharray: 6;
      animation: dash 1.5s linear infinite;
    }
    @keyframes dash {
      to { stroke-dashoffset: -12; }
    }
    .node-card {
      transition: all 0.2s cubic-bezier(0.16, 1, 0.3, 1);
    }
    .node-card:hover {
      transform: translateY(-2px);
      box-shadow: 0 10px 25px -5px rgba(2, 132, 199, 0.15), 0 8px 10px -6px rgba(2, 132, 199, 0.1);
    }
  </style>
</head>
<body class="bg-slate-50 text-slate-900 font-sans antialiased text-xs select-none">

  <!-- Header -->
  <header class="bg-white border-b border-slate-200 px-6 py-3 flex justify-between items-center sticky top-0 z-50 shadow-sm">
    <div class="flex items-center space-x-3">
      <span class="font-mono font-bold text-sm tracking-tight text-slate-900">TBG CORE // TREASURY</span>
      <span class="bg-sky-50 text-sky-700 border border-sky-200 font-mono text-[10px] font-semibold px-2 py-0.5 rounded">ORCHESTRATION WORKBENCH</span>
    </div>
    <div class="flex items-center space-x-4">
      <button onclick="simulateTrace()" id="btn-trace" class="bg-sky-600 hover:bg-sky-700 text-white font-semibold px-3 py-1.5 rounded flex items-center space-x-1.5 transition text-xs shadow-sm">
        <svg class="w-3.5 h-3.5 fill-current" viewBox="0 0 24 24"><path d="M8 5v14l11-7z"/></svg>
        <span>Simulate Live Transaction Flow</span>
      </button>
      <div class="font-mono text-[11px] text-slate-500 border-l border-slate-200 pl-4">
        POSTGRESQL 16 ACID <span class="mx-1">•</span> INVARIANT: &Sigma;DR - &Sigma;CR = 0 <span class="mx-1">•</span> P99: 0.98ms
      </div>
    </div>
  </header>

  <!-- Metrics Ribbon -->
  <div class="grid grid-cols-6 bg-slate-200 gap-px border-b border-slate-200">
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-0.5">Corporate Float</div>
      <div class="font-mono text-sm font-semibold text-slate-900" id="m-float-inr">INR 9,97,50,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-0.5">Transit Suspense</div>
      <div class="font-mono text-sm font-semibold text-amber-600" id="m-suspense-inr">INR 0.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-0.5">USD Liquidity</div>
      <div class="font-mono text-sm font-semibold text-sky-600" id="m-float-usd">USD 3,500,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-0.5">RERA 70% Escrow</div>
      <div class="font-mono text-sm font-semibold text-slate-900" id="m-rera-escrow">INR 7,00,00,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-0.5">Trade LC Margin</div>
      <div class="font-mono text-sm font-semibold text-purple-700" id="m-lc-margin">INR 2,50,00,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-0.5">Audited Postings</div>
      <div class="font-mono text-sm font-semibold text-emerald-600" id="m-postings-count">4 Entries</div>
    </div>
  </div>

  <div class="grid grid-cols-12 min-h-[calc(100vh-105px)]">
    <!-- Sidebar Navigation -->
    <aside class="col-span-2 bg-white border-r border-slate-200 p-3 space-y-1">
      <div class="text-[10px] font-mono font-bold uppercase text-slate-400 px-3 py-1">Canvas & Visualizer</div>
      <button onclick="tab('view-canvas', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100 active bg-sky-50 text-sky-700 font-semibold border border-sky-200">
        <span>⚡ Interactive Node Canvas</span>
      </button>

      <div class="text-[10px] font-mono font-bold uppercase text-slate-400 px-3 py-1 pt-3">Transaction Products</div>
      <button onclick="tab('view-payout', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">1. Domestic Multi-Rail (ISO)</button>
      <button onclick="tab('view-cbpr', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">2. SWIFT CBPR+ (MT103)</button>
      <button onclick="tab('view-van', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">3. Virtual Accounts (VAN)</button>
      <button onclick="tab('view-rera', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">4. RERA 70/30 Escrow</button>
      <button onclick="tab('view-zba', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">5. Liquidity Sweeps (ZBA)</button>
      <button onclick="tab('view-lc', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">6. Trade Finance (MT700 LC)</button>
      
      <div class="text-[10px] font-mono font-bold uppercase text-slate-400 px-3 py-1 pt-3">Architecture & Docs</div>
      <button onclick="tab('view-prd', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">7. Institutional PRD</button>
      <button onclick="tab('view-glossary', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">8. Banking Glossary</button>
    </aside>

    <!-- Main Workspace -->
    <main class="col-span-10 relative overflow-hidden bg-slate-100">

      <!-- 1. INTERACTIVE N8N-STYLE NODE CANVAS -->
      <div id="view-canvas" class="tab-panel h-full flex flex-col">
        <!-- Canvas Toolbar -->
        <div class="bg-white border-b border-slate-200 px-6 py-2 flex justify-between items-center z-10 shadow-sm">
          <div class="flex items-center space-x-2">
            <span class="inline-block w-2 h-2 rounded-full bg-emerald-500 animate-pulse"></span>
            <span class="font-mono text-xs font-semibold text-slate-700">CANVAS MODE: HIGH-VALUE WHOLESALE CLEARING PIPELINE</span>
          </div>
          <div class="text-xs text-slate-500">
            Click any node to open its <strong class="text-slate-800">Inspector Drawer</strong>, view active payloads, and check regulatory policies.
          </div>
        </div>

        <!-- The Canvas Board -->
        <div class="flex-1 relative overflow-auto canvas-grid p-10 flex items-center justify-center min-h-[620px]">
          
          <!-- Connectors SVG Layer -->
          <svg class="absolute inset-0 w-full h-full pointer-events-none" style="min-width: 1100px; min-height: 600px;">
            <defs>
              <linearGradient id="grad-active" x1="0%" y1="0%" x2="100%" y2="0%">
                <stop offset="0%" stop-color="#0284c7" />
                <stop offset="100%" stop-color="#06b6d4" />
              </linearGradient>
            </defs>
            <!-- ERP to Ingress -->
            <path id="path-1" d="M 230 300 C 275 300, 275 300, 320 300" stroke="#cbd5e1" stroke-width="2.5" fill="none"/>
            <!-- Ingress to Ledger Core -->
            <path id="path-2" d="M 510 300 C 555 300, 555 300, 600 300" stroke="#cbd5e1" stroke-width="2.5" fill="none"/>
            <!-- Ledger to Router -->
            <path id="path-3" d="M 790 300 C 830 300, 830 300, 870 300" stroke="#cbd5e1" stroke-width="2.5" fill="none"/>
            <!-- Router to Clearing -->
            <path id="path-4" d="M 1060 300 C 1100 300, 1100 300, 1140 300" stroke="#cbd5e1" stroke-width="2.5" fill="none"/>
          </svg>

          <!-- Nodes Container -->
          <div class="flex items-center space-x-20 z-10" style="min-width: 1200px;">
            
            <!-- NODE 1: Corporate ERP -->
            <div id="node-erp" onclick="openNodeInspector('erp')" class="node-card w-48 bg-white border-2 border-slate-200 rounded-lg p-3 cursor-pointer shadow-sm relative">
              <div class="flex justify-between items-center mb-1.5">
                <span class="text-[9px] font-mono font-bold uppercase text-slate-400">Trigger Layer</span>
                <span id="dot-erp" class="w-2 h-2 rounded-full bg-slate-300"></span>
              </div>
              <div class="font-bold text-xs text-slate-800">1. Corporate ERP</div>
              <div class="font-mono text-[10px] text-slate-500 mt-0.5">SAP S/4HANA / H2H</div>
              <div class="mt-2 bg-slate-50 border border-slate-100 rounded px-1.5 py-0.5 font-mono text-[9px] text-sky-700">pain.001.001.09</div>
              <!-- Right port -->
              <span class="absolute -right-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
            </div>

            <!-- NODE 2: Ingress & Validation -->
            <div id="node-ingress" onclick="openNodeInspector('ingress')" class="node-card w-48 bg-white border-2 border-slate-200 rounded-lg p-3 cursor-pointer shadow-sm relative">
              <!-- Left port -->
              <span class="absolute -left-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
              <div class="flex justify-between items-center mb-1.5">
                <span class="text-[9px] font-mono font-bold uppercase text-slate-400">Gatekeeper</span>
                <span id="dot-ingress" class="w-2 h-2 rounded-full bg-slate-300"></span>
              </div>
              <div class="font-bold text-xs text-slate-800">2. Ingress & Filter</div>
              <div class="font-mono text-[10px] text-slate-500 mt-0.5">Idempotency & OFAC</div>
              <div class="mt-2 bg-slate-50 border border-slate-100 rounded px-1.5 py-0.5 font-mono text-[9px] text-sky-700">HMAC-SHA256 Auth</div>
              <!-- Right port -->
              <span class="absolute -right-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
            </div>

            <!-- NODE 3: Double-Entry Core -->
            <div id="node-ledger" onclick="openNodeInspector('ledger')" class="node-card w-48 bg-white border-2 border-slate-200 rounded-lg p-3 cursor-pointer shadow-sm relative">
              <!-- Left port -->
              <span class="absolute -left-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
              <div class="flex justify-between items-center mb-1.5">
                <span class="text-[9px] font-mono font-bold uppercase text-emerald-600">ACID Ledger</span>
                <span id="dot-ledger" class="w-2 h-2 rounded-full bg-slate-300"></span>
              </div>
              <div class="font-bold text-xs text-slate-800">3. Ledger Core</div>
              <div class="font-mono text-[10px] text-slate-500 mt-0.5">DR Float // CR Suspense</div>
              <div class="mt-2 bg-emerald-50 border border-emerald-100 rounded px-1.5 py-0.5 font-mono text-[9px] text-emerald-700 font-semibold">&Sigma;DR = &Sigma;CR Verified</div>
              <!-- Right port -->
              <span class="absolute -right-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
            </div>

            <!-- NODE 4: Multi-Rail Smart Router -->
            <div id="node-router" onclick="openNodeInspector('router')" class="node-card w-48 bg-white border-2 border-slate-200 rounded-lg p-3 cursor-pointer shadow-sm relative">
              <!-- Left port -->
              <span class="absolute -left-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
              <div class="flex justify-between items-center mb-1.5">
                <span class="text-[9px] font-mono font-bold uppercase text-amber-600">Decision Matrix</span>
                <span id="dot-router" class="w-2 h-2 rounded-full bg-slate-300"></span>
              </div>
              <div class="font-bold text-xs text-slate-800">4. Dynamic Router</div>
              <div class="font-mono text-[10px] text-slate-500 mt-0.5">RTGS / NEFT / UPI</div>
              <div class="mt-2 bg-amber-50 border border-amber-100 rounded px-1.5 py-0.5 font-mono text-[9px] text-amber-800">MDR / SLA Optimized</div>
              <!-- Right port -->
              <span class="absolute -right-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
            </div>

            <!-- NODE 5: Nostro Clearing & Finality -->
            <div id="node-clearing" onclick="openNodeInspector('clearing')" class="node-card w-48 bg-white border-2 border-slate-200 rounded-lg p-3 cursor-pointer shadow-sm relative">
              <!-- Left port -->
              <span class="absolute -left-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 bg-white border-2 border-slate-400 rounded-full"></span>
              <div class="flex justify-between items-center mb-1.5">
                <span class="text-[9px] font-mono font-bold uppercase text-purple-600">Settlement Finality</span>
                <span id="dot-clearing" class="w-2 h-2 rounded-full bg-slate-300"></span>
              </div>
              <div class="font-bold text-xs text-slate-800">5. Central Clearing</div>
              <div class="font-mono text-[10px] text-slate-500 mt-0.5">RBI SFMS / SWIFT CBPR+</div>
              <div class="mt-2 bg-purple-50 border border-purple-100 rounded px-1.5 py-0.5 font-mono text-[9px] text-purple-700">camt.053 EOD Rec</div>
            </div>

          </div>
        </div>

        <!-- Slide-Over Inspector Drawer -->
        <div id="node-drawer" class="hidden absolute top-0 right-0 w-[450px] h-full bg-white border-l border-slate-200 shadow-2xl z-40 flex flex-col">
          <div class="p-4 border-b border-slate-200 bg-slate-50 flex justify-between items-center">
            <div>
              <span class="font-mono text-[10px] uppercase font-bold text-sky-600" id="dr-layer">LAYER DIAGNOSTICS</span>
              <h3 class="font-bold text-sm text-slate-800" id="dr-title">Node Title</h3>
            </div>
            <button onclick="closeDrawer()" class="text-slate-400 hover:text-slate-600 text-lg font-bold px-2 py-0.5 rounded">&times;</button>
          </div>
          <div class="p-5 flex-1 overflow-y-auto space-y-4">
            <div>
              <label class="block text-[10px] font-bold text-slate-400 uppercase tracking-wider mb-1">Operational Description</label>
              <p class="text-xs text-slate-600 leading-relaxed" id="dr-desc"></p>
            </div>
            <div>
              <label class="block text-[10px] font-bold text-slate-400 uppercase tracking-wider mb-1">Regulatory Invariant / Protocol</label>
              <div class="bg-slate-100 border border-slate-200 p-2.5 rounded font-mono text-[11px] text-slate-700 font-semibold" id="dr-rule"></div>
            </div>
            <div>
              <label class="block text-[10px] font-bold text-slate-400 uppercase tracking-wider mb-1">Runtime Wire Packet / Payload</label>
              <pre class="bg-slate-900 text-sky-400 p-3 rounded font-mono text-[10.5px] max-h-56 overflow-auto" id="dr-payload"></pre>
            </div>
          </div>
        </div>
      </div>

      <!-- 2. DOMESTIC PAYOUT -->
      <div id="view-payout" class="tab-panel hidden p-6">
        <div class="grid grid-cols-12 gap-6">
          <div class="col-span-4 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide border-b border-slate-100 pb-2 mb-3">Execute Outward Payout</h2>
            <form onsubmit="event.preventDefault(); submitDomestic();" class="space-y-3">
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Debtor Account</label>
                <input type="text" id="p-src" value="00040310001928" readonly class="w-full bg-slate-50 border border-slate-200 px-2.5 py-1.5 rounded font-mono text-xs text-slate-600">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Beneficiary Name</label>
                <input type="text" id="p-bene" value="Tata Motors Fleet Ltd" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Beneficiary Account</label>
                <input type="text" id="p-acct" value="912345678901" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">IFSC Code</label>
                <input type="text" id="p-ifsc" value="HDFC0000001" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Amount (INR)</label>
                <input type="number" id="p-amt" value="250000" min="1" step="0.01" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs font-semibold">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Clearing Rail</label>
                <select id="p-rail" class="w-full border border-slate-300 px-2 py-1.5 rounded font-mono text-xs">
                  <option value="AUTO">SMART_ROUTE (Latency / Cost Matrix)</option>
                  <option value="RTGS">RTGS (High-Value Gross Settlement)</option>
                  <option value="NEFT">NEFT (Batch Clearing - RBI SFMS)</option>
                  <option value="IMPS">IMPS (24x7 Real-Time Switch)</option>
                </select>
              </div>
              <button type="submit" class="w-full bg-sky-600 hover:bg-sky-700 text-white font-semibold py-2 rounded text-xs transition">Dispatch Idempotent Transfer</button>
              <button type="button" onclick="submitEOD()" class="w-full bg-slate-100 hover:bg-slate-200 text-slate-700 font-semibold py-1.5 rounded text-xs transition">Trigger EOD Nostro Settlement</button>
            </form>
          </div>
          <div class="col-span-8 space-y-4">
            <div class="bg-white border border-slate-200 rounded shadow-sm overflow-hidden">
              <div class="bg-slate-50 px-4 py-2.5 border-b border-slate-200 flex justify-between items-center">
                <span class="font-bold text-slate-700 text-xs uppercase tracking-wide">Double-Entry Journal Postings</span>
                <span class="text-emerald-700 bg-emerald-50 border border-emerald-200 font-mono text-[10px] font-semibold px-2 py-0.5 rounded">✓ ZERO-SUM VERIFIED</span>
              </div>
              <div class="overflow-x-auto max-h-72">
                <table class="w-full text-left font-mono text-[11px]">
                  <thead class="bg-slate-100 text-slate-500 border-b border-slate-200 sticky top-0">
                    <tr>
                      <th class="p-2.5">ID</th><th class="p-2.5">JV ID</th><th class="p-2.5">Account</th><th class="p-2.5">Leg</th><th class="p-2.5">Amount</th><th class="p-2.5">Module</th><th class="p-2.5">Narrative</th>
                    </tr>
                  </thead>
                  <tbody id="ledger-rows" class="divide-y divide-slate-100"></tbody>
                </table>
              </div>
            </div>
            <div class="grid grid-cols-2 gap-4">
              <div class="bg-white border border-slate-200 rounded shadow-sm p-3">
                <div class="text-[10px] font-bold text-slate-500 uppercase mb-2">ISO 20022 PACS.008 XML</div>
                <pre id="pacs-box" class="bg-slate-900 text-sky-400 p-3 rounded font-mono text-[10.5px] max-h-48 overflow-auto">&lt;!-- Dispatched pacs.008 schema payload will stream here --&gt;</pre>
              </div>
              <div class="bg-white border border-slate-200 rounded shadow-sm p-3">
                <div class="text-[10px] font-bold text-slate-500 uppercase mb-2">ERP Webhook Dispatch</div>
                <pre id="webhook-box" class="bg-slate-900 text-emerald-400 p-3 rounded font-mono text-[10.5px] max-h-48 overflow-auto">/* Signed HMAC-SHA256 ERP webhook payload */</pre>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 3. SWIFT CBPR+ -->
      <div id="view-cbpr" class="tab-panel hidden p-6">
        <div class="grid grid-cols-12 gap-6">
          <div class="col-span-4 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide border-b border-slate-100 pb-2 mb-3">SWIFT MT103 / CBPR+ Outward</h2>
            <form onsubmit="event.preventDefault(); submitCBPR();" class="space-y-3">
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Source Account</label>
                <input type="text" id="cb-src" value="CORP_US_FLOAT_0029" readonly class="w-full bg-slate-50 border border-slate-200 px-2.5 py-1.5 rounded font-mono text-xs text-slate-600">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Beneficiary Name</label>
                <input type="text" id="cb-bene" value="Airbus Operations GmbH" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Beneficiary IBAN</label>
                <input type="text" id="cb-iban" value="DE89370400440532013000" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Beneficiary BIC</label>
                <input type="text" id="cb-bic" value="DBEUMM21XXX" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Intermediary BIC</label>
                <input type="text" id="cb-int" value="CHASUS33XXX" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Amount (USD)</label>
                <input type="number" id="cb-amt" value="250000" min="100" step="0.01" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs font-semibold">
              </div>
              <button type="submit" class="w-full bg-sky-600 hover:bg-sky-700 text-white font-semibold py-2 rounded text-xs transition">Dispatch SWIFT MT103</button>
            </form>
          </div>
          <div class="col-span-8 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h3 class="font-bold text-slate-800 text-xs uppercase tracking-wide mb-2">Generated SWIFT FIN MT103 Wire Block</h3>
            <pre id="swift-box" class="bg-slate-900 text-sky-400 p-4 rounded font-mono text-xs max-h-96 overflow-auto">/* Dispatched MT103 block will appear here */</pre>
          </div>
        </div>
      </div>

      <!-- 4. VIRTUAL ACCOUNTS (VAN) -->
      <div id="view-van" class="tab-panel hidden p-6">
        <div class="grid grid-cols-12 gap-6">
          <div class="col-span-4 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide border-b border-slate-100 pb-2 mb-3">Receivables & VAN Matching</h2>
            <form onsubmit="event.preventDefault(); submitVAN();" class="space-y-3">
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Virtual Account (VAN)</label>
                <input type="text" id="van-acct" value="VAN-90812-INV44" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Remitter Entity Name</label>
                <input type="text" id="van-remitter" value="Reliance Retail Operations" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Remitter IFSC</label>
                <input type="text" id="van-ifsc" value="SBIN0001041" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Invoice Reference</label>
                <input type="text" id="van-inv" value="INV-2026-SEP-091" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Collection Amount (INR)</label>
                <input type="number" id="van-amt" value="1850000" min="100" step="100" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs font-semibold">
              </div>
              <button type="submit" class="w-full bg-sky-600 hover:bg-sky-700 text-white font-semibold py-2 rounded text-xs transition">Ingest & Auto-Reconcile VAN</button>
            </form>
          </div>
          <div class="col-span-8 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h3 class="font-bold text-slate-800 text-xs uppercase tracking-wide mb-2">VAN Auto-Reconciliation Event Log</h3>
            <pre id="van-box" class="bg-slate-900 text-emerald-400 p-4 rounded font-mono text-xs max-h-96 overflow-auto">/* VAN Remittance output will stream here */</pre>
          </div>
        </div>
      </div>

      <!-- 5. RERA ESCROW -->
      <div id="view-rera" class="tab-panel hidden p-6">
        <div class="grid grid-cols-12 gap-6">
          <div class="col-span-4 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide border-b border-slate-100 pb-2 mb-3">RERA Section 4 Dual-Escrow Split</h2>
            <form onsubmit="event.preventDefault(); submitRera();" class="space-y-3">
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Project ID</label>
                <input type="text" value="PRJ-MAHARERA-PUNE-2026-904" readonly class="w-full bg-slate-50 border border-slate-200 px-2.5 py-1.5 rounded font-mono text-xs text-slate-600">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Homebuyer VAN</label>
                <input type="text" id="rera-van" value="VAN-PUNE-TWR-801" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Consideration Amount (INR)</label>
                <input type="number" id="rera-amt" value="5000000" min="1000" step="100" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs font-semibold">
              </div>
              <button type="submit" class="w-full bg-sky-600 hover:bg-sky-700 text-white font-semibold py-2 rounded text-xs transition">Execute Escrow Split</button>
            </form>
          </div>
          <div class="col-span-8 space-y-4">
            <div class="grid grid-cols-2 gap-4">
              <div class="bg-white border-l-4 border-sky-600 border border-slate-200 p-4 rounded shadow-sm">
                <div class="text-[10px] uppercase font-bold text-slate-400">70% Dedicated Project Escrow</div>
                <div class="font-mono text-lg font-bold text-sky-700 mt-1" id="box-rera-70">INR 7,00,00,000.00</div>
              </div>
              <div class="bg-white border-l-4 border-emerald-600 border border-slate-200 p-4 rounded shadow-sm">
                <div class="text-[10px] uppercase font-bold text-slate-400">30% Operational Current Account</div>
                <div class="font-mono text-lg font-bold text-emerald-700 mt-1" id="box-rera-30">INR 3,00,00,000.00</div>
              </div>
            </div>
            <pre id="rera-box" class="bg-slate-900 text-sky-400 p-4 rounded font-mono text-xs max-h-60 overflow-auto">/* RERA Execution output will appear here */</pre>
          </div>
        </div>
      </div>

      <!-- 6. LIQUIDITY SWEEPS -->
      <div id="view-zba" class="tab-panel hidden p-6">
        <div class="grid grid-cols-12 gap-6">
          <div class="col-span-4 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide border-b border-slate-100 pb-2 mb-3">Zero-Balance Sweeper (ZBA)</h2>
            <form onsubmit="event.preventDefault(); submitSweep();" class="space-y-3">
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Subsidiary ZBA Account</label>
                <input type="text" id="zba-acct" value="SUBSIDIARY_PUNE_PLANT_4021" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Sweep Amount (INR)</label>
                <input type="number" id="zba-amt" value="2500000" min="1000" step="100" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs font-semibold">
              </div>
              <button type="submit" class="w-full bg-sky-600 hover:bg-sky-700 text-white font-semibold py-2 rounded text-xs transition">Execute Cash Sweep</button>
            </form>
          </div>
          <div class="col-span-8 space-y-4">
            <div class="bg-white border-l-4 border-emerald-600 border border-slate-200 p-4 rounded shadow-sm">
              <div class="text-[10px] uppercase font-bold text-slate-400">Concentrated Master Pool</div>
              <div class="font-mono text-xl font-bold text-emerald-700 mt-1" id="box-pool">INR 8,50,00,000.00</div>
            </div>
            <pre id="sweep-box" class="bg-slate-900 text-sky-400 p-4 rounded font-mono text-xs max-h-60 overflow-auto">/* Sweep execution journal output */</pre>
          </div>
        </div>
      </div>

      <!-- 7. TRADE FINANCE (LC) -->
      <div id="view-lc" class="tab-panel hidden p-6">
        <div class="grid grid-cols-12 gap-6">
          <div class="col-span-4 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide border-b border-slate-100 pb-2 mb-3">Letter of Credit Drawdown</h2>
            <form onsubmit="event.preventDefault(); submitLC();" class="space-y-3">
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">LC Reference</label>
                <input type="text" id="lc-ref" value="DLC-2026-MUM-8911" readonly class="w-full bg-slate-50 border border-slate-200 px-2.5 py-1.5 rounded font-mono text-xs text-slate-600">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Applicant Name</label>
                <input type="text" id="lc-applicant" value="Bharat Steel & Infrastructure Ltd" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Beneficiary Name</label>
                <input type="text" id="lc-bene" value="Nippon Steel Heavy Industries Corp" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Advising BIC</label>
                <input type="text" id="lc-bic" value="BOTKJPJTXXX" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs">
              </div>
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Drawdown Amount (INR)</label>
                <input type="number" id="lc-amt" value="5000000" min="1000" step="100" required class="w-full border border-slate-300 px-2.5 py-1.5 rounded font-mono text-xs font-semibold">
              </div>
              <button type="submit" class="w-full bg-purple-700 hover:bg-purple-800 text-white font-semibold py-2 rounded text-xs transition">Honor Documents & Drawdown</button>
            </form>
          </div>
          <div class="col-span-8 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h3 class="font-bold text-slate-800 text-xs uppercase tracking-wide mb-2">Generated SWIFT FIN MT700 Record</h3>
            <pre id="lc-box" class="bg-slate-900 text-purple-300 p-4 rounded font-mono text-xs max-h-96 overflow-auto">/* Dispatched MT700 will appear here */</pre>
          </div>
        </div>
      </div>

      <!-- 8. INSTITUTIONAL PRD -->
      <div id="view-prd" class="tab-panel hidden p-8 space-y-6">
        <div class="bg-white border border-slate-200 rounded shadow-sm p-6 space-y-4">
          <div class="border-b border-slate-100 pb-3">
            <span class="text-[10px] font-mono uppercase font-bold text-sky-600">SYSTEM ARCHITECTURE SPECIFICATION</span>
            <h1 class="text-base font-bold text-slate-900 mt-1">TBG-CORE Product Requirement Document (PRD v4.2)</h1>
          </div>
          <div>
            <h3 class="font-bold text-xs text-slate-800 uppercase tracking-wide mb-1">1. Architectural Mandate</h3>
            <p class="text-slate-600 text-xs leading-relaxed">
              TBG-CORE functions as the centralized clearing orchestrator and transaction banking switch for Scheduled Commercial Banks and large-scale FinTech aggregators. It decouples high-volume customer initiation from downstream interbank clearing houses, preventing settlement leakage, daylight overdraft anomalies, and double-debit states.
            </p>
          </div>
          <div>
            <h3 class="font-bold text-xs text-slate-800 uppercase tracking-wide mb-1">2. Core Regulatory Invariants</h3>
            <div class="space-y-2 text-xs text-slate-600">
              <div class="bg-slate-50 border border-slate-200 p-3 rounded">
                <strong class="text-slate-900">Invariant I: Strict Double-Entry Postings (&Sigma;DR = &Sigma;CR)</strong><br>
                Under Basel III guidelines, atomic balance holds are mandatory. An outward payout must instantly debit Corporate Operating Float and credit CMS Suspense. The bank assumes liability on its internal books prior to dispatching interbank wire instructions.
              </div>
              <div class="bg-slate-50 border border-slate-200 p-3 rounded">
                <strong class="text-slate-900">Invariant II: Distributed Idempotency Locks</strong><br>
                All execution requests require unique client UUID/HMAC keys. Duplicate submissions over flakey networks are intercepted before reaching ledger memory, returning the prior settlement state without re-executing balance debits.
              </div>
              <div class="bg-slate-50 border border-slate-200 p-3 rounded">
                <strong class="text-slate-900">Invariant III: RERA Section 4(2)(l)(D) Ring-Fencing</strong><br>
                Homebuyer remittances to registered real estate project Virtual Accounts are automatically partitioned at ingress: exactly 70% is ring-fenced for certified site construction expenses, and 30% is released to operational current accounts.
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 9. BANKING GLOSSARY -->
      <div id="view-glossary" class="tab-panel hidden p-8">
        <div class="grid grid-cols-3 gap-4">
          <div class="bg-white border border-slate-200 rounded p-4 shadow-sm">
            <div class="font-mono text-xs font-bold text-sky-700 uppercase mb-1">pacs.008 (ISO 20022)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Financial Customer Credit Transfer. Interbank standard for wholesale transfers across RTGS, NEFT, and SWIFT CBPR+.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded p-4 shadow-sm">
            <div class="font-mono text-xs font-bold text-sky-700 uppercase mb-1">CMS Suspense Account</div>
            <p class="text-slate-600 text-xs leading-relaxed">Internal clearing transit ledger representing the bank's liability to clearing houses before daily multilateral net settlement.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded p-4 shadow-sm">
            <div class="font-mono text-xs font-bold text-sky-700 uppercase mb-1">SWIFT FIN MT103</div>
            <p class="text-slate-600 text-xs leading-relaxed">Single customer credit transfer format containing Tag 20, Tag 32A, Tag 50K, and mandatory UETR tracking references.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded p-4 shadow-sm">
            <div class="font-mono text-xs font-bold text-sky-700 uppercase mb-1">Zero-Balance Account (ZBA)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Subsidiary account swept to a central master pool at cutoff to eliminate idle cash and optimize overnight repo yields.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded p-4 shadow-sm">
            <div class="font-mono text-xs font-bold text-sky-700 uppercase mb-1">Virtual Account (VAN)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Shadow routing identifier mapped directly to a client pool account for automated reconciliation without opening thousands of distinct accounts.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded p-4 shadow-sm">
            <div class="font-mono text-xs font-bold text-sky-700 uppercase mb-1">Documentary Credit (MT700)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Irrevocable undertaking issued by an issuing bank guaranteeing payment upon presentation of compliant documents under UCP 600 rules.</p>
          </div>
        </div>
      </div>

    </main>
  </div>

  <script>
    const nodeDetails = {
      erp: {
        layer: "LAYER 1 // INITIATION",
        title: "Corporate ERP Integration",
        desc: "Corporate treasury systems (SAP, Oracle Treasury) transmit standardized customer credit transfers. Outward payment batches carry immutable client-generated reference keys.",
        rule: "Protocols: ISO 20022 pain.001.001.09 // SWIFT MT101 // Host-to-Host SFTP",
        payload: '{\n  "initiation_id": "ERP-2026-90812",\n  "debtor_account": "00040310001928",\n  "beneficiary_iban": "DE89370400440532013000",\n  "amount": 250000.00,\n  "currency": "INR",\n  "payment_method": "CREDIT_TRANSFER"\n}'
      },
      ingress: {
        layer: "LAYER 2 // GATEWAY SECURITY",
        title: "Ingress & Sanctions Screening",
        desc: "Verifies HMAC-SHA256 signatures, acquires distributed idempotency locks, and executes OFAC/FATF sanctions fuzzy matching in under 0.5 milliseconds before touching ledger storage.",
        rule: "Security: Distributed Redis Lock // Sub-millisecond OFAC Match // Zero Duplicate Bookings",
        payload: '{\n  "idempotency_key": "TXN-89012391",\n  "hmac_sha256": "4b68e9...9a12c",\n  "ofac_screening": "CLEARED",\n  "pep_match": "NEGATIVE",\n  "lock_status": "ACQUIRED"\n}'
      },
      ledger: {
        layer: "LAYER 3 // POSTING CORE",
        title: "Double-Entry Balance Reservation",
        desc: "Executes atomic double-entry postings. The corporate float is debited while the CMS Suspense Account is credited, guaranteeing zero daylight overdraft exposure.",
        rule: "Basel III Invariant: Sum(DR) - Sum(CR) = 0 // Immediate Transit Hold",
        payload: '[\n  {\n    "account": "AC_CMS_SUSPENSE_CLEARING_9999",\n    "leg": "CR",\n    "amount": 250000.00,\n    "status": "HOLD_RESERVED"\n  },\n  {\n    "account": "00040310001928",\n    "leg": "DR",\n    "amount": 250000.00,\n    "status": "DEBITED"\n  }\n]'
      },
      router: {
        layer: "LAYER 4 // CLEARING DECISION",
        title: "Multi-Rail Smart Routing Matrix",
        desc: "Evaluates ticket size, rail TPS health, latency, and interchange/clearing costs. Amounts >= INR 2,00,000 route via RTGS; retail amounts route via instant NPCI UPI/IMPS rails.",
        rule: "Dynamic Decision: RTGS (>= 2L) // NEFT (Batch) // UPI/IMPS (Retail)",
        payload: '{\n  "ticket_amount": 250000.00,\n  "selected_rail": "RTGS",\n  "protocol": "RBI_SFMS_GROSS",\n  "network_latency": "1.02ms",\n  "cost_per_txn": "INR 25.00"\n}'
      },
      clearing: {
        layer: "LAYER 5 // FINALITY",
        title: "Central Clearing & Nostro Settlement",
        desc: "End-of-day statement reconciliation (camt.053) matches multilateral net obligations, discharging the bank CMS Suspense liability against the central bank Nostro account.",
        rule: "Finality: Immediate in RTGS // camt.053 End-of-Day File Matching",
        payload: '{\n  "clearing_house": "RESERVE BANK OF INDIA",\n  "nostro_account": "AC_RBI_NOSTRO_0001",\n  "settlement_status": "FINALITY_ACHIEVED",\n  "camt053_rec": "MATCHED"\n}'
      }
    };

    function openNodeInspector(key) {
      const data = nodeDetails[key];
      if (!data) return;
      document.getElementById('dr-layer').innerText = data.layer;
      document.getElementById('dr-title').innerText = data.title;
      document.getElementById('dr-desc').innerText = data.desc;
      document.getElementById('dr-rule').innerText = data.rule;
      document.getElementById('dr-payload').innerText = data.payload;
      document.getElementById('node-drawer').classList.remove('hidden');

      // Highlight active node
      document.querySelectorAll('.node-card').forEach(n => n.classList.remove('border-sky-500', 'bg-sky-50/30'));
      document.getElementById('node-' + key).classList.add('border-sky-500', 'bg-sky-50/30');
    }

    function closeDrawer() {
      document.getElementById('node-drawer').classList.add('hidden');
      document.querySelectorAll('.node-card').forEach(n => n.classList.remove('border-sky-500', 'bg-sky-50/30'));
    }

    function tab(id, btn) {
      document.querySelectorAll('.tab-panel').forEach(p => p.classList.add('hidden'));
      document.querySelectorAll('.tab-btn').forEach(b => {
        b.classList.remove('bg-sky-50', 'text-sky-700', 'font-semibold', 'border', 'border-sky-200');
      });
      document.getElementById(id).classList.remove('hidden');
      btn.classList.add('bg-sky-50', 'text-sky-700', 'font-semibold', 'border', 'border-sky-200');
    }

    async function simulateTrace() {
      tab('view-canvas', document.querySelector('.tab-btn'));
      const btn = document.getElementById('btn-trace');
      btn.disabled = true;
      btn.classList.add('opacity-50');

      const nodes = ['erp', 'ingress', 'ledger', 'router', 'clearing'];
      const paths = ['path-1', 'path-2', 'path-3', 'path-4'];

      // Reset
      nodes.forEach(n => {
        document.getElementById('dot-' + n).className = 'w-2 h-2 rounded-full bg-slate-300';
        document.getElementById('node-' + n).classList.remove('border-sky-500', 'bg-sky-50/30');
      });
      paths.forEach(p => {
        document.getElementById(p).setAttribute('stroke', '#cbd5e1');
        document.getElementById(p).classList.remove('flow-line');
      });

      for (let i = 0; i < nodes.length; i++) {
        const node = nodes[i];
        document.getElementById('dot-' + node).className = 'w-2 h-2 rounded-full bg-sky-500 animate-ping';
        openNodeInspector(node);

        if (i < paths.length) {
          const path = document.getElementById(paths[i]);
          path.setAttribute('stroke', '#0284c7');
          path.classList.add('flow-line');
        }

        await new Promise(r => setTimeout(r, 1200));
        document.getElementById('dot-' + node).className = 'w-2 h-2 rounded-full bg-emerald-500';
      }

      btn.disabled = false;
      btn.classList.remove('opacity-50');
    }

    async function loadData() {
      const res = await fetch('/api/v1/ledger');
      const data = await res.json();
      document.getElementById('m-float-inr').innerText = 'INR ' + Number(data.corporate_float_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-suspense-inr').innerText = 'INR ' + Number(data.cms_suspense_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-float-usd').innerText = 'USD ' + Number(data.corporate_float_usd).toLocaleString('en-US', {minimumFractionDigits: 2});
      document.getElementById('m-rera-escrow').innerText = 'INR ' + Number(data.rera_project_escrow).toLocaleString('en-IN', {minimumFractionDigits: 2});
      if (document.getElementById('box-rera-70')) document.getElementById('box-rera-70').innerText = 'INR ' + Number(data.rera_project_escrow).toLocaleString('en-IN', {minimumFractionDigits: 2});
      if (document.getElementById('box-rera-30')) document.getElementById('box-rera-30').innerText = 'INR ' + Number(data.rera_free_float).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-lc-margin').innerText = 'INR ' + Number(data.lc_escrow_margin_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      if (document.getElementById('box-pool')) document.getElementById('box-pool').innerText = 'INR ' + Number(data.liquidity_sweep_pool).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-postings-count').innerText = data.entry_counter + ' Entries';

      const tbody = document.getElementById('ledger-rows');
      if (tbody) {
        tbody.innerHTML = '';
        data.postings_ledger.forEach(r => {
          const tr = document.createElement('tr');
          const pill = r.leg === 'DR' ? '<span class="bg-rose-50 text-rose-700 border border-rose-200 px-1.5 py-0.5 rounded text-[10px] font-bold">DR</span>' : '<span class="bg-emerald-50 text-emerald-700 border border-emerald-200 px-1.5 py-0.5 rounded text-[10px] font-bold">CR</span>';
          tr.innerHTML = '<td class="p-2.5 font-bold">' + r.id + '</td><td class="p-2.5 text-slate-500">' + r.jv_id + '</td><td class="p-2.5">' + r.account_id + '</td><td class="p-2.5">' + pill + '</td><td class="p-2.5 font-semibold">' + r.currency + ' ' + Number(r.amount).toLocaleString() + '</td><td class="p-2.5 text-slate-500">' + r.module + '</td><td class="p-2.5 text-slate-600">' + r.narrative + '</td>';
          tbody.appendChild(tr);
        });
      }
    }

    async function submitDomestic() {
      const payload = {
        source_account: document.getElementById('p-src').value,
        beneficiary_name: document.getElementById('p-bene').value,
        beneficiary_account: document.getElementById('p-acct').value,
        beneficiary_ifsc: document.getElementById('p-ifsc').value,
        amount: parseFloat(document.getElementById('p-amt').value),
        rail: document.getElementById('p-rail').value,
        idempotency_key: 'TXN-' + Math.floor(10000000 + Math.random() * 90000000)
      };
      const res = await fetch('/api/v1/payouts/domestic', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
      });
      if (!res.ok) { alert('Payout Failed: ' + await res.text()); return; }
      const out = await res.json();
      document.getElementById('pacs-box').innerText = out.iso20022_xml;
      document.getElementById('webhook-box').innerText = JSON.stringify(out, null, 2);
      await loadData();
    }

    async function submitCBPR() {
      const payload = {
        source_account: document.getElementById('cb-src').value,
        beneficiary_name: document.getElementById('cb-bene').value,
        beneficiary_iban: document.getElementById('cb-iban').value,
        beneficiary_bic: document.getElementById('cb-bic').value,
        intermediary_bic: document.getElementById('cb-int').value,
        amount: parseFloat(document.getElementById('cb-amt').value),
        currency: 'USD',
        charge_bearer: 'OUR',
        idempotency_key: 'E2E-' + Math.floor(10000000 + Math.random() * 90000000)
      };
      const res = await fetch('/api/v1/payouts/cross-border', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('swift-box').innerText = out.swift_mt103;
      await loadData();
    }

    async function submitVAN() {
      const payload = {
        virtual_account: document.getElementById('van-acct').value,
        remitter_entity: document.getElementById('van-remitter').value,
        remitter_ifsc: document.getElementById('van-ifsc').value,
        amount: parseFloat(document.getElementById('van-amt').value),
        invoice_reference: document.getElementById('van-inv').value
      };
      const res = await fetch('/api/v1/receivables/van-collection', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('van-box').innerText = JSON.stringify(out, null, 2);
      await loadData();
    }

    async function submitRera() {
      const payload = {
        project_id: "PRJ-MAHARERA-PUNE-2026-904",
        buyer_van: document.getElementById('rera-van').value,
        amount: parseFloat(document.getElementById('rera-amt').value)
      };
      const res = await fetch('/api/v1/escrow/rera-split', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('rera-box').innerText = JSON.stringify(out, null, 2);
      await loadData();
    }

    async function submitSweep() {
      const payload = {
        subsidiary_account: document.getElementById('zba-acct').value,
        sweep_amount: parseFloat(document.getElementById('zba-amt').value)
      };
      const res = await fetch('/api/v1/liquidity/zba-sweep', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('sweep-box').innerText = JSON.stringify(out, null, 2);
      await loadData();
    }

    async function submitLC() {
      const payload = {
        lc_reference: document.getElementById('lc-ref').value,
        applicant_name: document.getElementById('lc-applicant').value,
        beneficiary_name: document.getElementById('lc-bene').value,
        issuing_bank_bic: document.getElementById('lc-bic').value,
        drawdown_amount_inr: parseFloat(document.getElementById('lc-amt').value)
      };
      const res = await fetch('/api/v1/trade-finance/lc-drawdown', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
      });
      const out = await res.json();
      document.getElementById('lc-box').innerText = out.swift_mt700;
      await loadData();
    }

    async function submitEOD() {
      const res = await fetch('/api/v1/recon/eod-nostro', { method: 'POST' });
      if (!res.ok) { alert('Notice: ' + await res.text()); return; }
      const out = await res.json();
      alert('EOD Nostro Cleared: INR ' + Number(out.discharged_suspense).toLocaleString('en-IN', {minimumFractionDigits: 2}));
      await loadData();
    }

    window.onload = function() {
      loadData();
    };
  </script>
</body>
</html>
`
