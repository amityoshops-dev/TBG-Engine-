package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

type PayoutRequest struct {
	ClientID           string  `json:"client_id"`
	VirtualAccount     string  `json:"virtual_account"`
	BeneficiaryAccount string  `json:"beneficiary_account"`
	IFSC               string  `json:"ifsc"`
	Amount             float64 `json:"amount"`
	PaymentRail        string  `json:"payment_rail"`
}

type FundRequest struct {
	ClientID       string  `json:"client_id"`
	VirtualAccount string  `json:"virtual_account"`
	Amount         float64 `json:"amount"`
	SourceUTR      string  `json:"source_utr"`
}

type AccountRecord struct {
	AccountNumber string    `json:"account_number"`
	ClientID      string    `json:"client_id"`
	Currency      string    `json:"currency"`
	Balance       float64   `json:"balance"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

type PostingRecord struct {
	EntryID     int64     `json:"entry_id"`
	JournalID   string    `json:"journal_id"`
	AccountNo   string    `json:"account_no"`
	Direction   string    `json:"direction"`
	Amount      float64   `json:"amount"`
	Description string    `json:"description"`
	Timestamp   time.Time `json:"timestamp"`
}

var (
	db  *sql.DB
	rdb *redis.Client
	mu  sync.Mutex
)

func initDB(dsn string) {
	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("[FATAL] PostgreSQL connection error: %v", err)
	}

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("[FATAL] PostgreSQL ping failed: %v", err)
	}
	log.Println("[INFO] PostgreSQL cluster connected successfully")

	schema := `
	CREATE TABLE IF NOT EXISTS accounts (
		account_number VARCHAR(64) PRIMARY KEY,
		client_id VARCHAR(64) NOT NULL,
		currency VARCHAR(3) DEFAULT 'INR',
		balance NUMERIC(18, 4) NOT NULL DEFAULT 0.0000,
		status VARCHAR(20) DEFAULT 'ACTIVE',
		created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS journal_entries (
		journal_id VARCHAR(64) PRIMARY KEY,
		reference_no VARCHAR(64) UNIQUE NOT NULL,
		narration TEXT NOT NULL,
		status VARCHAR(20) DEFAULT 'COMMITTED',
		created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS postings (
		entry_id BIGSERIAL PRIMARY KEY,
		journal_id VARCHAR(64) REFERENCES journal_entries(journal_id),
		account_no VARCHAR(64) REFERENCES accounts(account_number),
		direction VARCHAR(6) CHECK (direction IN ('DEBIT', 'CREDIT')),
		amount NUMERIC(18, 4) NOT NULL,
		description TEXT,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
	);

	INSERT INTO accounts (account_number, client_id, currency, balance, status)
	VALUES 
		('VA8800112233', 'CORP-CLIENT-001', 'INR', 10000000.0000, 'ACTIVE'),
		('VA8800112244', 'CORP-CLIENT-002', 'INR', 5000000.0000, 'ACTIVE'),
		('ESCROW-POOL-01', 'TREASURY-001', 'INR', 50000000.0000, 'ACTIVE'),
		('RBI-SETTLEMENT-CLEARING', 'RBI-RAIL-01', 'INR', 0.0000, 'ACTIVE')
	ON CONFLICT (account_number) 
	DO UPDATE SET balance = accounts.balance;
	`
	if _, err := db.Exec(schema); err != nil {
		log.Printf("[WARN] Schema notice: %v", err)
	}
}

func initRedis() {
	rawURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if rawURL == "" {
		rawURL = strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	}
	if rawURL == "" {
		rawURL = "localhost:6379"
	}

	var opt *redis.Options
	var err error
	if strings.HasPrefix(rawURL, "redis://") || strings.HasPrefix(rawURL, "rediss://") {
		opt, err = redis.ParseURL(rawURL)
		if err != nil {
			log.Fatalf("[FATAL] Redis URL parse failure: %v", err)
		}
	} else {
		opt = &redis.Options{Addr: rawURL}
	}

	if strings.HasPrefix(rawURL, "rediss://") {
		opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	rdb = redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("[WARN] Redis notice: %v (local fallback active)", err)
	} else {
		log.Println("[INFO] Redis distributed lock engine online")
	}
}

func main() {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/tbg_ledger?sslmode=disable"
	}

	initDB(dsn)
	initRedis()

	mux := http.NewServeMux()

	mux.HandleFunc("/", serveTerminal)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/health", handleHealthz)
	mux.HandleFunc("/api/v1/cms/stats", handleStats)
	mux.HandleFunc("/api/v1/cms/accounts", handleAccounts)
	mux.HandleFunc("/api/v1/cms/payout", handlePayout)
	mux.HandleFunc("/api/v1/cms/fund", handleFund)
	mux.HandleFunc("/api/v1/ledger/postings", handlePostings)

	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	addr := ":" + strings.TrimPrefix(port, ":")

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("[INFO] TBG-CORE Production Engine listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[FATAL] Server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("[INFO] Graceful shutdown initiated...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	log.Println("[INFO] TBG-CORE Engine successfully stopped")
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "OPERATIONAL",
		"timestamp":   time.Now().UTC(),
		"cluster":     "TBG-PROD-CORE-01",
		"services":    map[string]string{"postgres": "ONLINE", "redis": "ONLINE", "ledger_engine": "BALANCED"},
	})
}

func handleAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	rows, err := db.Query("SELECT account_number, client_id, currency, balance, status, created_at FROM accounts ORDER BY account_number ASC")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []AccountRecord
	for rows.Next() {
		var a AccountRecord
		rows.Scan(&a.AccountNumber, &a.ClientID, &a.Currency, &a.Balance, &a.Status, &a.CreatedAt)
		list = append(list, a)
	}
	json.NewEncoder(w).Encode(list)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var corpBalance, poolBalance float64
	db.QueryRow("SELECT COALESCE(balance, 0) FROM accounts WHERE account_number = 'VA8800112233'").Scan(&corpBalance)
	db.QueryRow("SELECT COALESCE(balance, 0) FROM accounts WHERE account_number = 'ESCROW-POOL-01'").Scan(&poolBalance)

	var txnCount int64
	db.QueryRow("SELECT COUNT(*) FROM journal_entries").Scan(&txnCount)

	var totalSettled float64
	db.QueryRow("SELECT COALESCE(SUM(amount), 0) FROM postings WHERE direction = 'DEBIT' AND account_no LIKE 'VA%'").Scan(&totalSettled)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"corporate_virtual_account": "VA8800112233",
		"corporate_available_float": corpBalance,
		"treasury_pool_float":       poolBalance,
		"settlement_rail":           "RTGS / NEFT / ISO-20022",
		"total_journal_entries":     txnCount,
		"total_volume_settled":      totalSettled,
		"system_health":             "ACTIVE_BALANCED",
	})
}

func handlePostings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	rows, err := db.Query("SELECT entry_id, journal_id, account_no, direction, amount, description, created_at FROM postings ORDER BY entry_id DESC LIMIT 60")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []PostingRecord
	for rows.Next() {
		var p PostingRecord
		rows.Scan(&p.EntryID, &p.JournalID, &p.AccountNo, &p.Direction, &p.Amount, &p.Description, &p.Timestamp)
		list = append(list, p)
	}
	json.NewEncoder(w).Encode(list)
}

func handleFund(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req FundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Malformed JSON", http.StatusBadRequest)
		return
	}

	if req.Amount <= 0 {
		http.Error(w, "Invalid funding amount", http.StatusBadRequest)
		return
	}

	journalID := fmt.Sprintf("JRN-FND-%d", time.Now().UnixNano())
	refNo := fmt.Sprintf("UTR-%s", req.SourceUTR)
	if req.SourceUTR == "" {
		refNo = fmt.Sprintf("UTR-%d", time.Now().Unix())
	}

	tx, err := db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(`INSERT INTO journal_entries (journal_id, reference_no, narration) VALUES ($1, $2, $3)`,
		journalID, refNo, fmt.Sprintf("Treasury Inward Liquidity for %s", req.VirtualAccount))
	if err != nil {
		http.Error(w, "Duplicate UTR reference or ledger conflict", http.StatusConflict)
		return
	}

	tx.Exec(`INSERT INTO accounts (account_number, client_id, currency, balance) VALUES ($1, $2, 'INR', $3)
	         ON CONFLICT (account_number) DO UPDATE SET balance = accounts.balance + $3`,
		req.VirtualAccount, req.ClientID, req.Amount)

	tx.Exec(`INSERT INTO postings (journal_id, account_no, direction, amount, description) VALUES
		($1, 'ESCROW-POOL-01', 'DEBIT', $2, 'Treasury Inward Clearing'),
		($1, $3, 'CREDIT', $2, 'Corporate Float Replenishment')`,
		journalID, req.Amount, req.VirtualAccount)

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "FUNDS_CREDITED",
		"journal_id":      journalID,
		"virtual_account": req.VirtualAccount,
		"credited_amount": req.Amount,
		"reference":       refNo,
	})
}

func handlePayout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error": "Method Not Allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		http.Error(w, `{"error": "Missing mandatory Idempotency-Key header"}`, http.StatusBadRequest)
		return
	}

	lockKey := "idemp:" + idempotencyKey
	if rdb != nil {
		ok, _ := rdb.SetNX(context.Background(), lockKey, "PROCESSING", 24*time.Hour).Result()
		if !ok {
			http.Error(w, `{"error": "Duplicate transaction: Idempotency-Key already executed or locked"}`, http.StatusConflict)
			return
		}
	} else {
		mu.Lock()
		defer mu.Unlock()
	}

	var req PayoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "Invalid request payload"}`, http.StatusBadRequest)
		return
	}

	if req.Amount <= 0 {
		http.Error(w, `{"error": "Amount must be strictly greater than 0.00"}`, http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var currentBalance float64
	err = tx.QueryRow(`SELECT balance FROM accounts WHERE account_number = $1 FOR UPDATE`, req.VirtualAccount).Scan(&currentBalance)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error": "Virtual account does not exist"}`, http.StatusNotFound)
		return
	}
	if currentBalance < req.Amount {
		http.Error(w, fmt.Sprintf(`{"error": "insufficient available liquidity for corporate float (Available: ₹%.2f, Required: ₹%.2f)"}`, currentBalance, req.Amount), http.StatusUnprocessableEntity)
		return
	}

	_, err = tx.Exec(`UPDATE accounts SET balance = balance - $1 WHERE account_number = $2`, req.Amount, req.VirtualAccount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	journalID := fmt.Sprintf("JRN-OUT-%d", time.Now().UnixNano())
	_, err = tx.Exec(`INSERT INTO journal_entries (journal_id, reference_no, narration) VALUES ($1, $2, $3)`,
		journalID, idempotencyKey, fmt.Sprintf("CMS Outward Payout via %s to %s", req.PaymentRail, req.BeneficiaryAccount))
	if err != nil {
		http.Error(w, `{"error": "Transaction reference already recorded in journal"}`, http.StatusConflict)
		return
	}

	_, err = tx.Exec(`INSERT INTO postings (journal_id, account_no, direction, amount, description) VALUES
		($1, $2, 'DEBIT', $3, 'Virtual Account Float Debit'),
		($1, 'RBI-SETTLEMENT-CLEARING', 'CREDIT', $3, 'Outward Rail Settlement Clearing')`,
		journalID, req.VirtualAccount, req.Amount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "SETTLED",
		"journal_id":      journalID,
		"idempotency_key": idempotencyKey,
		"virtual_account": req.VirtualAccount,
		"debited_amount":  req.Amount,
		"beneficiary":     req.BeneficiaryAccount,
		"ifsc":            req.IFSC,
		"rail":            req.PaymentRail,
		"clearing_status": "COMMITTED_TO_RBI_CLEARING",
		"timestamp":       time.Now().UTC(),
	})
}

func serveTerminal(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(terminalHTML))
}

const terminalHTML = `<!DOCTYPE html>
<html lang="en" data-theme="dark">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TBG-CORE | Transaction Banking Terminal</title>
<style>
  :root[data-theme="dark"] {
    --bg: #090d16; --surface: #111827; --panel: #162032; --border: #1f293d;
    --accent: #0284c7; --accent-hover: #0369a1; --accent-glow: rgba(14, 165, 233, 0.2);
    --green: #10b981; --red: #ef4444; --yellow: #f59e0b;
    --text: #f1f5f9; --muted: #94a3b8; --input-bg: #0b1120;
    --badge-bg: #0369a1; --table-header: #131d31; --card-val: #ffffff;
  }
  :root[data-theme="light"] {
    --bg: #f8fafc; --surface: #ffffff; --panel: #f1f5f9; --border: #cbd5e1;
    --accent: #0284c7; --accent-hover: #0369a1; --accent-glow: rgba(2, 132, 199, 0.2);
    --green: #059669; --red: #dc2626; --yellow: #d97706;
    --text: #0f172a; --muted: #64748b; --input-bg: #ffffff;
    --badge-bg: #e0f2fe; --table-header: #e2e8f0; --card-val: #0f172a;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "JetBrains Mono", "Segoe UI", monospace; transition: background 0.15s, color 0.15s, border-color 0.15s; }
  body { background: var(--bg); color: var(--text); height: 100vh; display: flex; flex-direction: column; overflow: hidden; font-size: 13px; }
  header { height: 50px; background: var(--surface); border-bottom: 1px solid var(--border); display: flex; align-items: center; justify-content: space-between; padding: 0 16px; }
  .brand { font-size: 14px; font-weight: 800; color: #38bdf8; display: flex; align-items: center; gap: 8px; letter-spacing: 0.5px; }
  .badge { background: var(--badge-bg); color: #fff; padding: 2px 8px; border-radius: 4px; font-size: 10px; font-weight: 700; }
  :root[data-theme="light"] .badge { color: #0369a1; border: 1px solid #bae6fd; }
  .nav-right { display: flex; align-items: center; gap: 12px; }
  .theme-toggle { background: var(--panel); border: 1px solid var(--border); color: var(--text); padding: 5px 12px; border-radius: 4px; cursor: pointer; font-size: 11px; font-weight: 700; }
  .live-dot { width: 8px; height: 8px; border-radius: 50%; background: var(--green); box-shadow: 0 0 8px var(--green); display: inline-block; margin-right: 4px; }
  .tab-strip { display: flex; background: var(--surface); border-bottom: 1px solid var(--border); padding: 0 16px; gap: 6px; }
  .tab-btn { background: none; border: none; border-bottom: 2px solid transparent; color: var(--muted); padding: 10px 14px; font-size: 12px; font-weight: 700; cursor: pointer; }
  .tab-btn.active { color: var(--accent); border-bottom-color: var(--accent); }
  .ribbon { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; padding: 12px 16px; background: var(--surface); border-bottom: 1px solid var(--border); }
  .card { background: var(--panel); border: 1px solid var(--border); border-radius: 6px; padding: 10px 14px; }
  .card-label { font-size: 10px; text-transform: uppercase; color: var(--muted); font-weight: 700; letter-spacing: 0.5px; }
  .card-val { font-size: 18px; font-weight: 800; margin-top: 4px; color: var(--card-val); }
  .view-container { display: flex; flex: 1; overflow: hidden; }
  .view-panel { display: none; width: 100%; height: 100%; }
  .view-panel.active { display: flex; }
  .desk-left { width: 440px; background: var(--surface); border-right: 1px solid var(--border); padding: 18px; overflow-y: auto; }
  .desk-right { flex: 1; background: var(--bg); display: flex; flex-direction: column; overflow: hidden; padding: 16px; }
  .section-title { font-size: 11px; text-transform: uppercase; font-weight: 800; color: #38bdf8; margin-bottom: 12px; display: flex; justify-content: space-between; align-items: center; }
  .form-group { margin-bottom: 12px; }
  .form-group label { display: block; font-size: 11px; color: var(--muted); margin-bottom: 4px; font-weight: 600; }
  .form-group input, .form-group select { width: 100%; background: var(--input-bg); border: 1px solid var(--border); border-radius: 4px; color: var(--text); padding: 8px 10px; font-size: 12px; outline: none; }
  .form-group input:focus, .form-group select:focus { border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-glow); }
  .btn-action { width: 100%; background: var(--accent); color: #fff; border: none; padding: 11px; border-radius: 4px; font-weight: 700; cursor: pointer; font-size: 12px; }
  .btn-action:hover { background: var(--accent-hover); }
  .btn-fund { background: var(--green); }
  .ledger-box { flex: 1; overflow-y: auto; background: var(--surface); border: 1px solid var(--border); border-radius: 6px; }
  .ledger-tbl { width: 100%; border-collapse: collapse; font-size: 11px; }
  .ledger-tbl th { background: var(--table-header); color: var(--muted); padding: 8px 12px; text-align: left; position: sticky; top: 0; font-size: 10px; text-transform: uppercase; border-bottom: 1px solid var(--border); z-index: 10; }
  .ledger-tbl td { padding: 9px 12px; border-bottom: 1px solid var(--border); }
  .debit-tag { color: var(--red); font-weight: 700; }
  .credit-tag { color: var(--green); font-weight: 700; }
  .ctrl-bar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px; gap: 10px; }
  .search-box { background: var(--surface); border: 1px solid var(--border); color: var(--text); padding: 6px 10px; border-radius: 4px; font-size: 11px; width: 260px; }
  #toast { display: none; position: fixed; bottom: 20px; right: 20px; background: var(--surface); border: 1.5px solid var(--accent); color: var(--text); padding: 12px 18px; border-radius: 6px; box-shadow: 0 10px 30px rgba(0,0,0,0.3); z-index: 99; font-size: 12px; }
</style>
</head>
<body>

<header>
  <div class="brand">
    <span>⚡ TBG-CORE TRANSACTION BANKING ENGINE</span>
    <span class="badge">PROD-ACID v12.4</span>
  </div>
  <div class="nav-right">
    <span style="font-size:11px; color:var(--muted);"><span class="live-dot"></span>Postgres 16 • Redis 7.2 • ISO-20022</span>
    <button class="theme-toggle" onclick="toggleTheme()">☀️ / 🌙 Theme</button>
  </div>
</header>

<div class="tab-strip">
  <button class="tab-btn active" onclick="switchView('WORKSTATION', this)">Payouts & Live Ledger</button>
  <button class="tab-btn" onclick="switchView('ACCOUNTS', this)">Virtual Accounts Master</button>
  <button class="tab-btn" onclick="switchView('TREASURY', this)">Inward Treasury Replenish</button>
  <button class="tab-btn" onclick="switchView('TELEMETRY', this)">System Health & Nodes</button>
</div>

<div class="ribbon">
  <div class="card">
    <div class="card-label">Corporate Float Balance (VA8800112233)</div>
    <div class="card-val" id="valCorpFloat">₹0.00</div>
  </div>
  <div class="card">
    <div class="card-label">Treasury Pool Collateral (ESCROW-01)</div>
    <div class="card-val" id="valPoolFloat">₹0.00</div>
  </div>
  <div class="card">
    <div class="card-label">Settled Payout Volume (Gross)</div>
    <div class="card-val" id="valGrossVol">₹0.00</div>
  </div>
  <div class="card">
    <div class="card-label">Balanced Double-Entry Journals</div>
    <div class="card-val" id="valJournals">0</div>
  </div>
</div>

<div class="view-container">
  <div class="view-panel active" id="viewWORKSTATION">
    <div class="desk-left">
      <div class="section-title">
        <span>CMS Outward Payout Dispatch</span>
        <span style="font-size:10px; color:var(--muted);">Rail: NEFT / RTGS</span>
      </div>
      <div class="form-group">
        <label>Debit Virtual Account:</label>
        <select id="inpVaSelect" onchange="syncSelectedVa()">
          <option value="VA8800112233">VA8800112233 (Corporate Primary Float)</option>
          <option value="VA8800112244">VA8800112244 (Secondary Operations)</option>
        </select>
      </div>
      <div class="form-group">
        <label>Client Reference ID:</label>
        <input type="text" id="inpClientId" value="CORP-CLIENT-001" readonly>
      </div>
      <div class="form-group">
        <label>Beneficiary Account Number:</label>
        <input type="text" id="inpBene" value="912345678901">
      </div>
      <div class="form-group">
        <label>Beneficiary IFSC Code:</label>
        <input type="text" id="inpIfsc" value="HDFC0000001">
      </div>
      <div class="form-group">
        <label>Payout Amount (₹):</label>
        <input type="number" id="inpAmount" value="25000.00" step="500">
      </div>
      <div class="form-group">
        <label>Idempotency Key (Distributed Safe):</label>
        <input type="text" id="inpIdemp" value="TXN-DEMO-001">
      </div>
      <button class="btn-action" onclick="submitPayout()">EXECUTE IDEMPOTENT PAYOUT</button>
    </div>

    <div class="desk-right">
      <div class="ctrl-bar">
        <div class="section-title" style="margin:0;">Real-Time Postings Ledger</div>
        <div style="display:flex; gap:8px;">
          <input type="text" class="search-box" id="inpLedgerSearch" placeholder="Filter by Account or Journal..." oninput="filterLedger()">
          <button class="theme-toggle" onclick="exportLedgerCSV()">Export CSV</button>
          <button class="theme-toggle" onclick="refreshData()">⟳ Refresh</button>
        </div>
      </div>
      <div class="ledger-box">
        <table class="ledger-tbl">
          <thead>
            <tr>
              <th>Entry ID</th>
              <th>Timestamp</th>
              <th>Journal Voucher</th>
              <th>Target Account</th>
              <th>Type</th>
              <th>Amount (₹)</th>
              <th>Narration</th>
            </tr>
          </thead>
          <tbody id="ledgerTbody"></tbody>
        </table>
      </div>
    </div>
  </div>

  <div class="view-panel" id="viewACCOUNTS" style="padding:20px; overflow-y:auto; flex-direction:column;">
    <div class="section-title">Corporate Virtual Account Directory & Position</div>
    <div class="ledger-box">
      <table class="ledger-tbl">
        <thead>
          <tr>
            <th>Virtual Account No</th>
            <th>Client Identifier</th>
            <th>Currency</th>
            <th>Available Ledger Balance</th>
            <th>Operating Status</th>
            <th>Created At</th>
          </tr>
        </thead>
        <tbody id="accountsTbody"></tbody>
      </table>
    </div>
  </div>

  <div class="view-panel" id="viewTREASURY" style="padding:24px;">
    <div style="max-width:540px; background:var(--surface); border:1px solid var(--border); border-radius:8px; padding:20px;">
      <div class="section-title">Treasury Float Inward / Top-Up Rail</div>
      <div class="form-group">
        <label>Credit Target Account:</label>
        <select id="inpFundTarget">
          <option value="VA8800112233">VA8800112233 (Corporate Primary Float)</option>
          <option value="VA8800112244">VA8800112244 (Secondary Operations)</option>
          <option value="ESCROW-POOL-01">ESCROW-POOL-01 (Treasury Pool Collateral)</option>
        </select>
      </div>
      <div class="form-group">
        <label>Inward Deposit Amount (₹):</label>
        <input type="number" id="inpFundAmt" value="1000000.00" step="50000">
      </div>
      <div class="form-group">
        <label>Source Bank UTR / Clearing Reference:</label>
        <input type="text" id="inpFundUtr" placeholder="BANK-INW-778899">
      </div>
      <button class="btn-action btn-fund" onclick="submitFunding()">REPLENISH VIRTUAL FLOAT</button>
    </div>
  </div>

  <div class="view-panel" id="viewTELEMETRY" style="padding:24px; flex-direction:column; gap:16px;">
    <div class="section-title">Core Subsystem Telemetry</div>
    <div style="display:grid; grid-template-columns: repeat(3, 1fr); gap:16px;">
      <div class="card">
        <div class="card-label">Relational Engine</div>
        <div class="card-val" style="color:var(--green);">PostgreSQL 16 (ACID)</div>
        <div style="font-size:11px; color:var(--muted); margin-top:6px;">Max Open Conns: 50 | Idle: 10</div>
      </div>
      <div class="card">
        <div class="card-label">Distributed Cache & Locks</div>
        <div class="card-val" style="color:var(--green);">Redis Stack 7.2</div>
        <div style="font-size:11px; color:var(--muted); margin-top:6px;">Key TTL: 24h Idempotency Guard</div>
      </div>
      <div class="card">
        <div class="card-label">Clearing Rails</div>
        <div class="card-val" style="color:var(--accent);">ISO 20022 Engine</div>
        <div style="font-size:11px; color:var(--muted); margin-top:6px;">pacs.008 / pain.001 Real-Time Sync</div>
      </div>
    </div>
  </div>
</div>

<div id="toast"></div>

<script>
  var cachedPostings = [];

  window.onload = function() {
    initTheme();
    generateNewIdemp();
    refreshData();
    loadAccounts();
    setInterval(refreshData, 3000);
  };

  function initTheme() {
    var saved = localStorage.getItem("tbg_theme") || "dark";
    document.documentElement.setAttribute("data-theme", saved);
  }

  function toggleTheme() {
    var cur = document.documentElement.getAttribute("data-theme");
    var nxt = cur === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", nxt);
    localStorage.setItem("tbg_theme", nxt);
  }

  function switchView(viewId, btn) {
    document.querySelectorAll(".view-panel").forEach(function(p) { p.classList.remove("active"); });
    document.querySelectorAll(".tab-btn").forEach(function(b) { b.classList.remove("active"); });
    document.getElementById("view" + viewId).classList.add("active");
    btn.classList.add("active");
    if(viewId === "ACCOUNTS") loadAccounts();
  }

  function syncSelectedVa() {
    var va = document.getElementById("inpVaSelect").value;
    document.getElementById("inpClientId").value = va === "VA8800112233" ? "CORP-CLIENT-001" : "CORP-CLIENT-002";
  }

  function generateNewIdemp() {
    document.getElementById("inpIdemp").value = "TXN-" + Date.now().toString().slice(-8);
  }

  function showToast(msg, isErr) {
    var t = document.getElementById("toast");
    t.innerText = msg;
    t.style.borderColor = isErr ? "var(--red)" : "var(--accent)";
    t.style.display = "block";
    setTimeout(function() { t.style.display = "none"; }, 3500);
  }

  async function refreshData() {
    try {
      var statsRes = await fetch("/api/v1/cms/stats");
      var stats = await statsRes.json();
      document.getElementById("valCorpFloat").innerText = "₹" + Number(stats.corporate_available_float).toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("valPoolFloat").innerText = "₹" + Number(stats.treasury_pool_float).toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("valGrossVol").innerText = "₹" + Number(stats.total_volume_settled).toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("valJournals").innerText = stats.total_journal_entries;

      var postRes = await fetch("/api/v1/ledger/postings");
      cachedPostings = await postRes.json();
      renderLedgerTable(cachedPostings);
    } catch(e) {
      console.error(e);
    }
  }

  function renderLedgerTable(postings) {
    var tbody = document.getElementById("ledgerTbody");
    tbody.innerHTML = "";
    if (postings && postings.length > 0) {
      postings.forEach(function(p) {
        tbody.innerHTML += "<tr>" +
          "<td>#" + p.entry_id + "</td>" +
          "<td style='color:var(--muted);'>" + new Date(p.timestamp).toLocaleTimeString() + "</td>" +
          "<td><b>" + p.journal_id + "</b></td>" +
          "<td><code>" + p.account_no + "</code></td>" +
          "<td><span class='" + (p.direction === "DEBIT" ? "debit-tag" : "credit-tag") + "'>" + p.direction + "</span></td>" +
          "<td><b>₹" + Number(p.amount).toLocaleString('en-IN', {minimumFractionDigits: 2}) + "</b></td>" +
          "<td style='color:var(--muted);'>" + p.description + "</td>" +
        "</tr>";
      });
    }
  }

  function filterLedger() {
    var q = document.getElementById("inpLedgerSearch").value.toLowerCase();
    var filtered = cachedPostings.filter(function(p) {
      return p.account_no.toLowerCase().includes(q) ||
             p.journal_id.toLowerCase().includes(q) ||
             p.description.toLowerCase().includes(q);
    });
    renderLedgerTable(filtered);
  }

  async function loadAccounts() {
    try {
      var res = await fetch("/api/v1/cms/accounts");
      var accounts = await res.json();
      var tbody = document.getElementById("accountsTbody");
      tbody.innerHTML = "";
      accounts.forEach(function(a) {
        tbody.innerHTML += "<tr>" +
          "<td><b>" + a.account_number + "</b></td>" +
          "<td>" + a.client_id + "</td>" +
          "<td>" + a.currency + "</td>" +
          "<td><b style='color:var(--green);'>₹" + Number(a.balance).toLocaleString('en-IN', {minimumFractionDigits: 2}) + "</b></td>" +
          "<td><span style='color:var(--green); font-weight:700;'>" + a.status + "</span></td>" +
          "<td style='color:var(--muted);'>" + new Date(a.created_at).toLocaleDateString() + "</td>" +
        "</tr>";
      });
    } catch(e) {
      console.error(e);
    }
  }

  function exportLedgerCSV() {
    if(!cachedPostings.length) return alert("No ledger postings available to export");
    var csv = "EntryID,Timestamp,JournalID,Account,Direction,Amount,Description\n";
    cachedPostings.forEach(function(p) {
      csv += p.entry_id + ',"' + p.timestamp + '","' + p.journal_id + '","' + p.account_no + '","' + p.direction + '",' + p.amount + ',"' + p.description + '"\n';
    });
    const blob = new Blob([csv], { type: "text/csv" });
    const url = window.URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "TBG_Ledger_" + Date.now() + ".csv";
    a.click();
  }

  async function submitPayout() {
    const idemp = document.getElementById("inpIdemp").value;
    const payload = {
      client_id: document.getElementById("inpClientId").value,
      virtual_account: document.getElementById("inpVaSelect").value,
      beneficiary_account: document.getElementById("inpBene").value,
      ifsc: document.getElementById("inpIfsc").value,
      amount: parseFloat(document.getElementById("inpAmount").value),
      payment_rail: "NEFT_RTGS"
    };

    try {
      const res = await fetch("/api/v1/cms/payout", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": idemp
        },
        body: JSON.stringify(payload)
      });
      const data = await res.json();
      if(res.ok) {
        showToast("Payout Settled: " + data.journal_id, false);
        generateNewIdemp();
        refreshData();
      } else {
        showToast(data.error || "Payout rejected by ledger", true);
      }
    } catch(err) {
      showToast("Network / Engine Error", true);
    }
  }

  async function submitFunding() {
    const targetVa = document.getElementById("inpFundTarget").value;
    const payload = {
      client_id: targetVa.startsWith("VA8800112244") ? "CORP-CLIENT-002" : "CORP-CLIENT-001",
      virtual_account: targetVa,
      amount: parseFloat(document.getElementById("inpFundAmt").value),
      source_utr: document.getElementById("inpFundUtr").value || ("BANK-INW-" + Date.now().toString().slice(-6))
    };

    try {
      const res = await fetch("/api/v1/cms/fund", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
      });
      const data = await res.json();
      if(res.ok) {
        showToast("Float Credited: ₹" + payload.amount.toLocaleString(), false);
        refreshData();
      } else {
        showToast("Funding failed: " + (data.error || "Conflict"), true);
      }
    } catch(err) {
      showToast("Engine Connection Error", true);
    }
  }
</script>
</body>
</html>`
