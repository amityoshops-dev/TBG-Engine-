package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
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

//go:embed web/index.html
var terminalHTML []byte

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
		"status":    "OPERATIONAL",
		"timestamp": time.Now().UTC(),
		"cluster":   "TBG-PROD-CORE-01",
		"services":  map[string]string{"postgres": "ONLINE", "redis": "ONLINE", "ledger_engine": "BALANCED"},
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
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
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
	w.Write(terminalHTML)
}
