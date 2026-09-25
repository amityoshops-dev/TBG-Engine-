package service

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"tbg-engine/internal/iso20022"
	"tbg-engine/internal/ledger"
	"tbg-engine/internal/observability"
)

const (
	SuspenseAccount = "AC_CMS_SUSPENSE_CLEARING_9999"
	NostroAccount   = "AC_RBI_NOSTRO_0001"
	IdempotencyTTL  = 24 * time.Hour
)

// PayoutService orchestrates the full payout lifecycle: idempotency check ->
// lock-free lien reservation -> double-entry ledger posting -> ISO 20022
// message generation -> lien release. This is the "INITIATED -> LIEN_HELD ->
// JOURNAL_POSTED -> IN_FLIGHT -> SETTLED" state machine collapsed into a
// single synchronous request for the reference implementation; a production
// deployment would split the post-lien steps across a Kafka consumer to
// decouple ingestion from settlement.
type PayoutService struct {
	Repo     *ledger.Repository
	Lien     *ledger.LienEngine
	RDB      *redis.Client
	Log      *observability.Logger
	HMACSalt string
}

func NewPayoutService(repo *ledger.Repository, lien *ledger.LienEngine, rdb *redis.Client, logger *observability.Logger, hmacSalt string) *PayoutService {
	return &PayoutService{Repo: repo, Lien: lien, RDB: rdb, Log: logger, HMACSalt: hmacSalt}
}

func (s *PayoutService) HandlePayout(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	correlationID := uuid.New().String()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idempKey := r.Header.Get("Idempotency-Key")
	if idempKey == "" {
		http.Error(w, "missing Idempotency-Key header", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// 1. Idempotency check: SETNX with 24h TTL. On collision, replay the
	// cached response so retried payouts never double-post.
	cacheKey := "idemp:" + idempKey
	set, err := s.RDB.SetNX(ctx, cacheKey, "PENDING", IdempotencyTTL).Result()
	if err != nil {
		http.Error(w, "idempotency store unavailable", http.StatusInternalServerError)
		return
	}
	if !set {
		cached, err := s.RDB.Get(ctx, cacheKey).Result()
		if err == nil && cached != "PENDING" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(cached))
			return
		}
		http.Error(w, "duplicate transaction detected (idempotency collision)", http.StatusConflict)
		return
	}

	var req ledger.PayoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.RDB.Del(ctx, cacheKey)
		http.Error(w, "malformed JSON payload", http.StatusBadRequest)
		return
	}

	// 2. Optional non-repudiation check: verify HMAC/SHA-512 signature if the
	// caller supplied one. (A production edge would terminate mTLS and
	// verify an RSA signature in an HSM before the request ever reaches
	// this handler; HMAC here is the same non-repudiation *pattern* using a
	// pre-shared key, matching the Postman collection's pre-request script.)
	if sig := r.Header.Get("X-Signature"); sig != "" {
		if !s.verifySignature(req, idempKey, sig) {
			s.RDB.Del(ctx, cacheKey)
			s.Log.Emit(observability.LogEntry{
				CorrelationID: correlationID, EventType: "SIGNATURE_VERIFICATION_FAILED",
				Status: "REJECTED", ErrorMessage: "HMAC signature mismatch",
			})
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
	}

	// 3. Lock-free lien reservation against corporate float liquidity.
	held, err := s.Lien.Hold(ctx, req.CorporateAccount, req.ReferenceID, req.Amount)
	if err != nil {
		s.RDB.Del(ctx, cacheKey)
		http.Error(w, "lien engine error", http.StatusInternalServerError)
		return
	}
	if !held {
		s.RDB.Del(ctx, cacheKey)
		s.Log.Emit(observability.LogEntry{
			CorrelationID: correlationID, EventType: "LIEN_REJECTED", Status: "FAILED",
			AccountID: req.CorporateAccount, Amount: req.Amount, Currency: req.Currency,
			ErrorMessage: "insufficient available liquidity",
		})
		http.Error(w, "insufficient available liquidity for corporate float", http.StatusUnprocessableEntity)
		return
	}

	// 4. Double-entry ledger posting (DR corporate float / CR CMS suspense),
	// committed atomically in PostgreSQL with the zero-sum invariant
	// enforced by the database trigger from init.sql.
	jvID := uuid.New().String()
	merkleHash, err := s.Repo.PostDoubleEntry(ctx, jvID, idempKey, req.CorporateAccount, SuspenseAccount, req.Amount)
	if err != nil {
		_ = s.Lien.Release(ctx, req.CorporateAccount, req.ReferenceID, true) // restore held funds on failure
		s.RDB.Del(ctx, cacheKey)
		s.Log.Emit(observability.LogEntry{
			CorrelationID: correlationID, EventType: "LEDGER_POSTING_FAILED", Status: "FAILED",
			AccountID: req.CorporateAccount, Amount: req.Amount, ErrorMessage: err.Error(),
		})
		http.Error(w, "ledger posting failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 5. ISO 20022 pacs.008 message generation for the downstream clearing
	// rail. In this reference implementation the rail dispatch itself is a
	// stub (`dispatchToClearingRail`) — wire it to a real SFMS/NPCI adapter
	// for production use.
	utr := fmt.Sprintf("CMS%s%d", req.PaymentRail, time.Now().UnixNano()%10_000_000_000)
	xmlPayload, err := iso20022.BuildPacs008(
		"MSG-"+jvID[:8], req.ReferenceID, "Corporate Enterprise Client",
		req.BeneficiaryName, req.BeneficiaryAcct, req.BeneficiaryIFSC, req.Currency, req.Amount,
	)
	if err != nil {
		s.Log.Emit(observability.LogEntry{
			CorrelationID: correlationID, EventType: "ISO20022_SERIALIZATION_FAILED",
			ErrorMessage: err.Error(),
		})
	} else {
		dispatchToClearingRail(xmlPayload)
	}

	// 6. Release the lien permanently — funds have moved from available
	// balance to the ledger, so no restoration is needed.
	_ = s.Lien.Release(ctx, req.CorporateAccount, req.ReferenceID, false)

	resp := ledger.PayoutResponse{
		Status:     "SETTLED",
		JVID:       jvID,
		UTR:        utr,
		MerkleHash: merkleHash,
		LedgerEntries: []ledger.LedgerEntryView{
			{AccountID: req.CorporateAccount, Direction: "DR", Amount: req.Amount},
			{AccountID: SuspenseAccount, Direction: "CR", Amount: req.Amount},
		},
		SettledAt: time.Now().UTC(),
	}

	respBytes, _ := json.Marshal(resp)
	s.RDB.Set(ctx, cacheKey, string(respBytes), IdempotencyTTL)

	latency := float64(time.Since(start).Microseconds()) / 1000.0
	s.Log.Emit(observability.LogEntry{
		CorrelationID: correlationID, EventType: "LEDGER_POSTING", Status: "SETTLED",
		AccountID: req.CorporateAccount, TxnID: jvID, Amount: req.Amount,
		Currency: req.Currency, LatencyMS: latency,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(respBytes)
}

// dispatchToClearingRail is a stub representing the outbound call to the
// bank's RBI SFMS / NPCI switch adapter. Replace with a real client.
func dispatchToClearingRail(pacs008XML []byte) {
	_ = pacs008XML
}

func (s *PayoutService) verifySignature(req ledger.PayoutRequest, idempKey, providedSig string) bool {
	raw := fmt.Sprintf("%s|%s|%f|%s", req.CorporateAccount, idempKey, req.Amount, s.HMACSalt)
	mac := hmac.New(sha512.New, []byte(s.HMACSalt))
	mac.Write([]byte(raw))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(providedSig))
}

// HandleStats exposes recent ledger postings and the live suspense balance
// for an audit / reconciliation dashboard.
func (s *PayoutService) HandleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	postings, err := s.Repo.RecentPostings(ctx, 20)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	suspenseBal, _ := s.Repo.SuspenseBalance(ctx, SuspenseAccount)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"postings":         postings,
		"suspense_balance": suspenseBal,
	})
}

// HandleHealthz is a liveness/readiness probe for orchestration platforms
// (Kubernetes, Render, etc.) — checks Redis connectivity as a fast proxy for
// dependency health.
func (s *PayoutService) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.RDB.Ping(ctx).Err(); err != nil {
		http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
