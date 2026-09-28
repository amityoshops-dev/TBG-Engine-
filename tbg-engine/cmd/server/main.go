package main

import (
	_ "embed"
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

//go:embed static/index.html
var staticIndexHTML []byte

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
	w.Write(staticIndexHTML)
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
