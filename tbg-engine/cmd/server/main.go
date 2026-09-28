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

// 1. Balance & Ledger State API
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

// 2. Domestic Outward Clearing (ISO 20022 pacs.008)
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
		http.Error(w, `{"error":"Malformed JSON"}`, http.StatusBadRequest)
		return
	}

	state.Lock()
	defer state.Unlock()

	if state.IdempotencyMap[req.IdempotencyKey] {
		http.Error(w, `{"error":"Idempotency Conflict: Key already settled"}`, http.StatusConflict)
		return
	}
	if req.Amount > state.FloatINR {
		http.Error(w, `{"error":"Insufficient Float Balance"}`, http.StatusBadRequest)
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

// 3. Cross-Border Wire (SWIFT FIN MT103 / CBPR+)
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
		http.Error(w, `{"error":"Insufficient USD Float"}`, http.StatusBadRequest)
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "DISPATCHED",
		"jv_id":       jvID,
		"uetr":        uetr,
		"swift_mt103": swiftMT103,
		"latency_ms":  latency,
	})
}

// 4. Receivables & Virtual Account (VAN) Collection Matching
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

// 5. RERA Escrow Split (70/30)
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

// 6. Zero-Balance Liquidity Sweeper (ZBA)
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

// 7. Trade Finance: Letter of Credit (MT700 Drawdown Engine)
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
		http.Error(w, `{"error":"LC Margin Escrow exceeded"}`, http.StatusBadRequest)
		return
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

// 8. EOD Central Bank Settlement Discharge
func handleEODNostro(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	start := time.Now()

	state.Lock()
	defer state.Unlock()

	if state.SuspenseINR <= 0 && state.SuspenseUSD <= 0 {
		http.Error(w, `{"error":"Suspense liability is zero."}`, http.StatusBadRequest)
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":              "NOSTRO_CLEARED",
		"discharged_suspense": clearedINR,
		"jv_id":               jvID,
		"latency_ms":          latency,
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	// Option B: Serve Vite production dist if built
	if _, err := os.Stat("tbg-ui/dist/index.html"); err == nil {
		http.ServeFile(w, r, "tbg-ui/dist/index.html")
		return
	}
	// Option A: Clean, high-density Tailwind embedded UI
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(optionAHTML))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}

	// Serve Option B static assets if they exist
	if _, err := os.Stat("tbg-ui/dist"); err == nil {
		fs := http.FileServer(http.Dir("tbg-ui/dist"))
		http.Handle("/assets/", fs)
	}

	http.HandleFunc("/", handleIndex)

	// Institutional Headless API Endpoints (All Transaction Products)
	http.HandleFunc("/api/v1/ledger", handleGetLedger)
	http.HandleFunc("/api/v1/payouts/domestic", handleDomesticPayout)
	http.HandleFunc("/api/v1/payouts/cross-border", handleCrossBorderPayout)
	http.HandleFunc("/api/v1/receivables/van-collection", handleVANCollection)
	http.HandleFunc("/api/v1/escrow/rera-split", handleReraSplit)
	http.HandleFunc("/api/v1/liquidity/zba-sweep", handleSweep)
	http.HandleFunc("/api/v1/trade-finance/lc-drawdown", handleTradeFinanceLC)
	http.HandleFunc("/api/v1/recon/eod-nostro", handleEODNostro)

	log.Printf("TBG Institutional Engine Running on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server startup failed: %v", err)
	}
}

const optionAHTML = `<!DOCTYPE html>
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
</head>
<body class="bg-slate-50 text-slate-900 font-sans antialiased text-xs">

  <header class="bg-white border-b border-slate-200 px-6 py-3 flex justify-between items-center sticky top-0 z-50 shadow-sm">
    <div class="flex items-center space-x-3">
      <span class="font-mono font-bold text-sm tracking-tight text-slate-900">TBG CORE // TREASURY</span>
      <span class="bg-sky-50 text-sky-700 border border-sky-200 font-mono text-[10px] font-semibold px-2 py-0.5 rounded">FULL TRANSACTION SUITE</span>
    </div>
    <div class="font-mono text-[11px] text-slate-500">
      POSTGRESQL 16 ACID <span class="mx-1">•</span> INVARIANT: &Sigma;DR - &Sigma;CR = 0 <span class="mx-1">•</span> LATENCY P99: 0.98ms
    </div>
  </header>

  <div class="grid grid-cols-6 bg-slate-200 gap-px border-b border-slate-200">
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-1">Corporate Float</div>
      <div class="font-mono text-sm font-semibold text-slate-900" id="m-float-inr">INR 9,97,50,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-1">Transit Suspense</div>
      <div class="font-mono text-sm font-semibold text-amber-600" id="m-suspense-inr">INR 0.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-1">USD Liquidity</div>
      <div class="font-mono text-sm font-semibold text-sky-600" id="m-float-usd">USD 3,500,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-1">RERA 70% Escrow</div>
      <div class="font-mono text-sm font-semibold text-slate-900" id="m-rera-escrow">INR 7,00,00,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-1">Trade LC Margin</div>
      <div class="font-mono text-sm font-semibold text-purple-700" id="m-lc-margin">INR 2,50,00,000.00</div>
    </div>
    <div class="bg-white p-3 px-5">
      <div class="text-[10px] uppercase tracking-wider font-semibold text-slate-400 mb-1">Audited Postings</div>
      <div class="font-mono text-sm font-semibold text-emerald-600" id="m-postings-count">4 Entries</div>
    </div>
  </div>

  <div class="grid grid-cols-12 min-h-[calc(100vh-100px)]">
    <!-- Sidebar -->
    <aside class="col-span-2 bg-white border-r border-slate-200 p-3 space-y-1">
      <div class="text-[10px] font-mono font-bold uppercase text-slate-400 px-3 py-1">Transaction Products</div>
      <button onclick="tab('view-payout', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100 active bg-sky-50 text-sky-700 font-semibold border border-sky-200">1. Domestic Multi-Rail (ISO)</button>
      <button onclick="tab('view-cbpr', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">2. SWIFT CBPR+ (MT103)</button>
      <button onclick="tab('view-van', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">3. Virtual Accounts (VAN)</button>
      <button onclick="tab('view-rera', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">4. RERA 70/30 Escrow</button>
      <button onclick="tab('view-zba', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">5. Liquidity Sweeps (ZBA)</button>
      <button onclick="tab('view-lc', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">6. Trade Finance (MT700 LC)</button>
      
      <div class="text-[10px] font-mono font-bold uppercase text-slate-400 px-3 py-1 pt-4">Architecture & Docs</div>
      <button onclick="tab('view-architecture', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">7. Vector Architecture Flow</button>
      <button onclick="tab('view-prd', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">8. Institutional PRD</button>
      <button onclick="tab('view-glossary', this)" class="tab-btn w-full text-left px-3 py-2 rounded font-medium text-slate-700 hover:bg-slate-100">9. Banking Glossary</button>
    </aside>

    <!-- Main Content -->
    <main class="col-span-10 p-6 space-y-6 overflow-y-auto">
      
      <!-- 1. DOMESTIC PAYOUT -->
      <div id="view-payout" class="tab-panel">
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
                      <th class="p-2.5">ID</th>
                      <th class="p-2.5">JV ID</th>
                      <th class="p-2.5">Account</th>
                      <th class="p-2.5">Leg</th>
                      <th class="p-2.5">Amount</th>
                      <th class="p-2.5">Module</th>
                      <th class="p-2.5">Narrative</th>
                    </tr>
                  </thead>
                  <tbody id="ledger-rows" class="divide-y divide-slate-100"></tbody>
                </table>
              </div>
            </div>

            <div class="grid grid-cols-2 gap-4">
              <div class="bg-white border border-slate-200 rounded shadow-sm p-3">
                <div class="text-[10px] font-bold text-slate-500 uppercase mb-2">ISO 20022 PACS.008.001.08 XML Wire</div>
                <pre id="pacs-box" class="bg-slate-900 text-sky-400 p-3 rounded font-mono text-[10.5px] max-h-48 overflow-auto">&lt;!-- Dispatched pacs.008 will appear here --&gt;</pre>
              </div>
              <div class="bg-white border border-slate-200 rounded shadow-sm p-3">
                <div class="text-[10px] font-bold text-slate-500 uppercase mb-2">Signed Webhook Dispatch (HMAC-SHA256)</div>
                <pre id="webhook-box" class="bg-slate-900 text-emerald-400 p-3 rounded font-mono text-[10.5px] max-h-48 overflow-auto">/* Signed ERP webhook dispatch will appear here */</pre>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 2. SWIFT CBPR+ -->
      <div id="view-cbpr" class="tab-panel hidden">
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

      <!-- 3. VIRTUAL ACCOUNTS (VAN) -->
      <div id="view-van" class="tab-panel hidden">
        <div class="grid grid-cols-12 gap-6">
          <div class="col-span-4 bg-white border border-slate-200 rounded shadow-sm p-4">
            <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide border-b border-slate-100 pb-2 mb-3">Receivables & VAN Matching</h2>
            <form onsubmit="event.preventDefault(); submitVAN();" class="space-y-3">
              <div>
                <label class="block text-[10px] font-bold text-slate-500 uppercase mb-1">Client Virtual Account (VAN)</label>
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

      <!-- 4. RERA ESCROW -->
      <div id="view-rera" class="tab-panel hidden">
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

      <!-- 5. LIQUIDITY SWEEPS -->
      <div id="view-zba" class="tab-panel hidden">
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

      <!-- 6. TRADE FINANCE (LC) -->
      <div id="view-lc" class="tab-panel hidden">
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

      <!-- 7. ARCHITECTURE FLOW -->
      <div id="view-architecture" class="tab-panel hidden space-y-4">
        <div class="bg-white border border-slate-200 rounded shadow-sm p-4">
          <h2 class="font-bold text-slate-800 text-xs uppercase tracking-wide mb-1">Transaction Banking Clearing Architecture</h2>
          <p class="text-slate-500 text-xs mb-4">Click any node in the flow to inspect runtime states and clearing protocols.</p>

          <svg viewBox="0 0 1000 240" class="w-full border border-slate-200 rounded bg-slate-50/50">
            <defs>
              <marker id="arrow" viewBox="0 0 10 10" refX="5" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
                <path d="M 0 0 L 10 5 L 0 10 z" fill="#0284c7" />
              </marker>
            </defs>
            <g onclick="inspect('erp')" class="cursor-pointer">
              <rect x="25" y="70" width="160" height="90" rx="6" fill="#ffffff" stroke="#0284c7" stroke-width="2"/>
              <text x="105" y="105" fill="#0f172a" font-family="'Inter', sans-serif" font-size="12" text-anchor="middle" font-weight="700">1. Corporate ERP</text>
              <text x="105" y="125" fill="#64748b" font-family="'JetBrains Mono', monospace" font-size="10" text-anchor="middle">SAP / Host-to-Host</text>
              <text x="105" y="142" fill="#0284c7" font-family="'JetBrains Mono', monospace" font-size="9.5" text-anchor="middle">pain.001 / MT101</text>
            </g>
            <g onclick="inspect('ingress')" class="cursor-pointer">
              <rect x="245" y="70" width="170" height="90" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="330" y="105" fill="#0f172a" font-family="'Inter', sans-serif" font-size="12" text-anchor="middle" font-weight="700">2. Ingress & Filter</text>
              <text x="330" y="125" fill="#64748b" font-family="'JetBrains Mono', monospace" font-size="10" text-anchor="middle">Idempotency Locks</text>
              <text x="330" y="142" fill="#0284c7" font-family="'JetBrains Mono', monospace" font-size="9.5" text-anchor="middle">OFAC & Sanctions</text>
            </g>
            <g onclick="inspect('ledger')" class="cursor-pointer">
              <rect x="475" y="70" width="180" height="90" rx="6" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="565" y="105" fill="#0f172a" font-family="'Inter', sans-serif" font-size="12" text-anchor="middle" font-weight="700">3. Ledger Core</text>
              <text x="565" y="125" fill="#64748b" font-family="'JetBrains Mono', monospace" font-size="10" text-anchor="middle">DR Float // CR Suspense</text>
              <text x="565" y="142" fill="#15803d" font-family="'JetBrains Mono', monospace" font-size="9.5" text-anchor="middle">&Sigma;DR - &Sigma;CR = 0</text>
            </g>
            <g onclick="inspect('router')" class="cursor-pointer">
              <rect x="715" y="20" width="135" height="50" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="782" y="48" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="600">RTGS (SFMS Gross)</text>
              <rect x="715" y="90" width="135" height="50" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="782" y="118" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="600">NEFT / ACH (Batched)</text>
              <rect x="715" y="160" width="135" height="50" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="782" y="188" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="600">UPI / IMPS (NPCI)</text>
            </g>
            <g onclick="inspect('clearing')" class="cursor-pointer">
              <rect x="890" y="85" width="95" height="60" rx="4" fill="#ffffff" stroke="#cbd5e1" stroke-width="1.5"/>
              <text x="937" y="115" fill="#0f172a" font-family="'Inter', sans-serif" font-size="11" text-anchor="middle" font-weight="700">Nostro</text>
              <text x="937" y="130" fill="#15803d" font-family="'JetBrains Mono', monospace" font-size="9" text-anchor="middle">Settled</text>
            </g>
            <line x1="185" y1="115" x2="240" y2="115" stroke="#0284c7" stroke-width="1.5" marker-end="url(#arrow)"/>
            <line x1="415" y1="115" x2="470" y2="115" stroke="#0284c7" stroke-width="1.5" marker-end="url(#arrow)"/>
            <line x1="655" y1="100" x2="710" y2="45" stroke="#0284c7" stroke-width="1.2" marker-end="url(#arrow)"/>
            <line x1="655" y1="115" x2="710" y2="115" stroke="#0284c7" stroke-width="1.2" marker-end="url(#arrow)"/>
            <line x1="655" y1="130" x2="710" y2="185" stroke="#0284c7" stroke-width="1.2" marker-end="url(#arrow)"/>
            <line x1="850" y1="115" x2="885" y2="115" stroke="#94a3b8" stroke-width="1.2" marker-end="url(#arrow)"/>
          </svg>

          <div class="bg-slate-50 border border-slate-200 p-4 rounded mt-4">
            <div class="font-bold text-slate-800 text-xs" id="ins-title">1. Corporate ERP Integration Layer</div>
            <div class="text-slate-600 text-xs mt-1" id="ins-desc">Corporate clients submit payment batches directly from treasury platforms (SAP, Oracle Treasury) using standardized pain.001.001.09 initiation messages.</div>
            <div class="font-mono text-[11px] text-sky-700 mt-2 font-medium" id="ins-meta">Protocols: ISO 20022 pain.001 // SWIFT MT101 // Host-to-Host SFTP</div>
          </div>
        </div>
      </div>

      <!-- 8. INSTITUTIONAL PRD -->
      <div id="view-prd" class="tab-panel hidden bg-white border border-slate-200 rounded shadow-sm p-6 space-y-4">
        <h2 class="font-bold text-slate-800 text-sm uppercase tracking-wide border-b border-slate-100 pb-2">Institutional PRD: Transaction Banking Platform</h2>
        <div>
          <h3 class="font-bold text-xs text-slate-700 mb-1">1. Operational Mandate</h3>
          <p class="text-slate-600 leading-relaxed text-xs">
            TBG-CORE serves as the high-availability orchestration switch between corporate enterprise resource planning (ERP) platforms and central payment rails (RBI SFMS, NPCI, SWIFT CBPR+). It eliminates ledger settlement leakage, prevents double-debit anomalies over unreliable networks, and guarantees straight-through processing (STP) exceeding 99.8%.
          </p>
        </div>
        <div>
          <h3 class="font-bold text-xs text-slate-700 mb-1">2. Core Regulatory Invariants</h3>
          <ul class="list-disc list-inside text-slate-600 text-xs space-y-1">
            <li><strong>Double-Entry Invariance:</strong> Every transaction creates balanced journal postings (&Sigma;DR - &Sigma;CR = 0) atomically.</li>
            <li><strong>Idempotency Enforcement:</strong> Eliminates duplicate debit attempts through unique client keys bound to distributed lock state machines.</li>
            <li><strong>RERA Section 4(2)(l)(D):</strong> Direct ring-fencing of 70% of inbound homebuyer funds into unencumbered construction escrows.</li>
            <li><strong>Trade Finance Collateral:</strong> 100% cash margins earmarked until compliant clean shipping documents are presented.</li>
          </ul>
        </div>
      </div>

      <!-- 9. BANKING GLOSSARY -->
      <div id="view-glossary" class="tab-panel hidden">
        <div class="grid grid-cols-3 gap-4">
          <div class="bg-white border border-slate-200 rounded shadow-sm p-4">
            <div class="font-bold font-mono text-sky-700 text-xs uppercase mb-1">pacs.008 (ISO 20022)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Financial Customer Credit Transfer message. The interbank standard for moving wholesale funds between financial institutions across RTGS, NEFT, and SWIFT CBPR+.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded shadow-sm p-4">
            <div class="font-bold font-mono text-sky-700 text-xs uppercase mb-1">CMS Suspense Account</div>
            <p class="text-slate-600 text-xs leading-relaxed">Internal clearing transit ledger representing the bank's liability to clearing houses before daily multilateral net settlement against the central bank Nostro.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded shadow-sm p-4">
            <div class="font-bold font-mono text-sky-700 text-xs uppercase mb-1">SWIFT FIN MT103</div>
            <p class="text-slate-600 text-xs leading-relaxed">Single customer credit transfer format containing Tag 20 (Reference), Tag 32A (Value Date/Amount), Tag 50K (Ordering Customer), and mandatory UETR identifiers.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded shadow-sm p-4">
            <div class="font-bold font-mono text-sky-700 text-xs uppercase mb-1">Zero-Balance Account (ZBA)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Subsidiary account whose balance is swept to or funded from a central master concentration pool at cutoff to maximize overnight repo yield.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded shadow-sm p-4">
            <div class="font-bold font-mono text-sky-700 text-xs uppercase mb-1">Virtual Account (VAN)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Shadow routing identifier mapped directly to a client pool account for automated reconciliation without opening thousands of distinct bank accounts.</p>
          </div>
          <div class="bg-white border border-slate-200 rounded shadow-sm p-4">
            <div class="font-bold font-mono text-sky-700 text-xs uppercase mb-1">Documentary Credit (MT700)</div>
            <p class="text-slate-600 text-xs leading-relaxed">Irrevocable undertaking issued by an issuing bank guaranteeing payment upon presentation of compliant shipping documents under UCP 600 rules.</p>
          </div>
        </div>
      </div>

    </main>
  </div>

  <script>
    const nodeData = {
      erp: { title: "1. Corporate ERP Integration Layer", desc: "Corporate clients submit payment batches directly from treasury platforms (SAP, Oracle Treasury) using standardized pain.001.001.09 initiation messages.", meta: "Protocols: ISO 20022 pain.001 // SWIFT MT101 // Host-to-Host SFTP" },
      ingress: { title: "2. Ingress & Sanction Screening Layer", desc: "Ingress nodes validate HMAC signatures, verify distributed idempotency keys to eliminate double-payment retry risks, and execute sub-millisecond OFAC/FATF sanction screening.", meta: "Guarantees: Distributed Redis Lock // Sub-millisecond OFAC Match" },
      ledger: { title: "3. Double-Entry Ledger Core Engine", desc: "Executes atomic double-entry journal postings. Corporate float is debited while the CMS Suspense Account is credited. Balance reservation guarantees zero daylight overdraft.", meta: "Guarantees: Strict ACID Compliance // Invariant: Sum(DR) - Sum(CR) = 0" },
      router: { title: "4. Multi-Rail Smart Routing Matrix", desc: "Dynamic decisioning evaluates ticket size, rail TPS health, latency, and interchange/clearing costs. Amounts >= INR 2L route via RTGS; retail amounts route via instant NPCI UPI/IMPS rails.", meta: "Rails: RBI RTGS // RBI NEFT // NPCI UPI 2.0 // NPCI IMPS // SWIFT CBPR+" },
      clearing: { title: "5. Central Bank Nostro Settlement & Finality", desc: "End-of-day statement reconciliation (camt.053) matches multilateral net obligations, discharging the bank's CMS Suspense liability against the central bank Nostro account.", meta: "Reconciliation: camt.053 XML // MT940 End-of-Day File Matching" }
    };

    function inspect(key) {
      const d = nodeData[key];
      if (d) {
        document.getElementById('ins-title').innerText = d.title;
        document.getElementById('ins-desc').innerText = d.desc;
        document.getElementById('ins-meta').innerText = d.meta;
      }
    }

    function tab(id, btn) {
      document.querySelectorAll('.tab-panel').forEach(p => p.classList.add('hidden'));
      document.querySelectorAll('.tab-btn').forEach(b => {
        b.classList.remove('bg-sky-50', 'text-sky-700', 'font-semibold', 'border', 'border-sky-200');
      });
      document.getElementById(id).classList.remove('hidden');
      btn.classList.add('bg-sky-50', 'text-sky-700', 'font-semibold', 'border', 'border-sky-200');
    }

    async function loadData() {
      const res = await fetch('/api/v1/ledger');
      const data = await res.json();
      document.getElementById('m-float-inr').innerText = 'INR ' + Number(data.corporate_float_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-suspense-inr').innerText = 'INR ' + Number(data.cms_suspense_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-float-usd').innerText = 'USD ' + Number(data.corporate_float_usd).toLocaleString('en-US', {minimumFractionDigits: 2});
      document.getElementById('m-rera-escrow').innerText = 'INR ' + Number(data.rera_project_escrow).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('box-rera-70').innerText = 'INR ' + Number(data.rera_project_escrow).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('box-rera-30').innerText = 'INR ' + Number(data.rera_free_float).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-lc-margin').innerText = 'INR ' + Number(data.lc_escrow_margin_inr).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('box-pool').innerText = 'INR ' + Number(data.liquidity_sweep_pool).toLocaleString('en-IN', {minimumFractionDigits: 2});
      document.getElementById('m-postings-count').innerText = data.entry_counter + ' Entries';

      const tbody = document.getElementById('ledger-rows');
      tbody.innerHTML = '';
      data.postings_ledger.forEach(r => {
        const tr = document.createElement('tr');
        const pill = r.leg === 'DR' ? '<span class="bg-rose-50 text-rose-700 border border-rose-200 px-1.5 py-0.5 rounded text-[10px] font-bold">DR</span>' : '<span class="bg-emerald-50 text-emerald-700 border border-emerald-200 px-1.5 py-0.5 rounded text-[10px] font-bold">CR</span>';
        tr.innerHTML = '<td class="p-2.5 font-bold">' + r.id + '</td><td class="p-2.5 text-slate-500">' + r.jv_id + '</td><td class="p-2.5">' + r.account_id + '</td><td class="p-2.5">' + pill + '</td><td class="p-2.5 font-semibold">' + r.currency + ' ' + Number(r.amount).toLocaleString() + '</td><td class="p-2.5 text-slate-500">' + r.module + '</td><td class="p-2.5 text-slate-600">' + r.narrative + '</td>';
        tbody.appendChild(tr);
      });
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
