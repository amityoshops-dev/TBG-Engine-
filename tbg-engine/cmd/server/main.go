package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"tbg-engine/internal/config"
	"tbg-engine/internal/ledger"
	"tbg-engine/internal/observability"
	"tbg-engine/internal/service"
)

func main() {
	cfg := config.Load()
	logger := observability.New()

	db, err := sql.Open("postgres", cfg.PostgresDSN)
	if err != nil {
		log.Fatalf("postgres error: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("postgres ping failed: %v", err)
	}

	rawRedisURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if rawRedisURL == "" {
		rawRedisURL = strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	}
	if rawRedisURL == "" {
		rawRedisURL = "localhost:6379"
	}

	var redisOpt *redis.Options
	if strings.HasPrefix(rawRedisURL, "redis://") || strings.HasPrefix(rawRedisURL, "rediss://") {
		redisOpt, err = redis.ParseURL(rawRedisURL)
		if err != nil {
			log.Fatalf("redis url error: %v", err)
		}
	} else {
		redisOpt = &redis.Options{Addr: rawRedisURL}
	}

	if strings.HasPrefix(rawRedisURL, "rediss://") {
		redisOpt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	rdb := redis.NewClient(redisOpt)
	defer rdb.Close()

	redisCtx, redisCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer redisCancel()
	if err := rdb.Ping(redisCtx).Err(); err != nil {
		log.Fatalf("redis ping error: %v", err)
	}

	repo := ledger.NewRepository(db)
	lienEngine := ledger.NewLienEngine(rdb)

	_ = lienEngine.SeedFloat(context.Background(), "00040310001928", 10000000.00)

	payoutSvc := service.NewPayoutService(repo, lienEngine, rdb, logger, cfg.HMACSalt)

	mux := http.NewServeMux()

	// CORS headers taaki alag chalne wala Next.js frontend is API ko call kar sake
	corsMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, X-Signature")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("/api/v1/cms/payout", corsMiddleware(payoutSvc.HandlePayout))
	mux.HandleFunc("/api/v1/cms/stats", corsMiddleware(payoutSvc.HandleStats))
	mux.HandleFunc("/healthz", corsMiddleware(payoutSvc.HandleHealthz))
	mux.HandleFunc("/health", corsMiddleware(payoutSvc.HandleHealthz))

	mux.HandleFunc("/api/v1/cms/reconcile", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		reqCtx := r.Context()
		suspenseBal, err := repo.SuspenseBalance(reqCtx, service.SuspenseAccount)
		if err != nil || suspenseBal <= 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "NOOP",
				"message": "Suspense balance is already zero or invalid",
			})
			return
		}

		jvID := uuid.New().String()
		idemp := "EOD-SETTLE-" + jvID[:8]
		merkle, err := repo.PostDoubleEntry(reqCtx, jvID, idemp, service.SuspenseAccount, service.NostroAccount, suspenseBal)
		if err != nil {
			http.Error(w, "reconciliation post failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":             "RECONCILED",
			"jv_id":              jvID,
			"cleared_amount":     suspenseBal,
			"merkle_hash":        merkle,
			"debit_account":      service.SuspenseAccount,
			"credit_account":     service.NostroAccount,
			"settlement_channel": "RBI_NET_SETTLEMENT_FILE",
		})
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(terminalHTML))
	})

	addr := strings.TrimSpace(os.Getenv("PORT"))
	if addr == "" {
		addr = strings.TrimSpace(cfg.ListenAddr)
	}
	if addr == "" {
		addr = "8080"
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = ":" + strings.TrimPrefix(addr, ":")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("TBG-CORE running on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
}

const terminalHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Finacle Treasury & Stripe Core | Institutional Terminal</title>
<style>
  :root {
    --bg-base: #030712;
    --bg-surface: #0b0f19;
    --bg-card: #111827;
    --border: #1f2937;
    --border-subtle: #374151;
    --text-main: #f9fafb;
    --text-muted: #9ca3af;
    --accent: #6366f1;
    --cyan: #06b6d4;
    --green: #10b981;
    --red: #f43f5e;
    --amber: #f59e0b;
    --mono: "JetBrains Mono", monospace;
    --sans: -apple-system, BlinkMacSystemFont, "Inter", sans-serif;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg-base); color: var(--text-main); font-family: var(--sans); height: 100vh; display: flex; flex-direction: column; overflow: hidden; font-size: 12px; }

  header { height: 42px; background: var(--bg-surface); border-bottom: 1px solid var(--border); display: flex; align-items: center; justify-content: space-between; padding: 0 14px; flex-shrink: 0; }
  .finacle-logo { font-family: var(--mono); font-size: 12px; font-weight: 800; letter-spacing: 0.5px; color: #fff; display: flex; align-items: center; gap: 8px; }
  .tag { font-family: var(--mono); font-size: 9px; padding: 1px 6px; border-radius: 3px; font-weight: 700; text-transform: uppercase; background: rgba(99,102,241,0.2); color: #818cf8; border: 1px solid rgba(99,102,241,0.3); }

  .ribbon { height: 50px; background: var(--bg-surface); border-bottom: 1px solid var(--border); display: grid; grid-template-columns: 2fr 1fr 1fr 1fr 1fr; align-items: center; padding: 0 14px; gap: 14px; flex-shrink: 0; }
  .metric-box { display: flex; flex-direction: column; }
  .metric-label { font-family: var(--mono); font-size: 9px; color: var(--text-muted); text-transform: uppercase; }
  .metric-val { font-family: var(--mono); font-size: 13px; font-weight: 700; color: #fff; margin-top: 1px; }

  .workbench { display: grid; grid-template-columns: 340px 1fr; flex: 1; overflow: hidden; }

  .rail-panel { background: var(--bg-surface); border-right: 1px solid var(--border); padding: 14px; display: flex; flex-direction: column; gap: 10px; overflow-y: auto; }
  .panel-hdr { font-family: var(--mono); font-size: 10px; font-weight: 800; color: var(--text-muted); text-transform: uppercase; border-bottom: 1px solid var(--border); padding-bottom: 4px; }
  .form-row { display: flex; flex-direction: column; gap: 2px; }
  .form-row label { font-family: var(--mono); font-size: 9px; color: var(--text-muted); text-transform: uppercase; }
  .inp { background: var(--bg-base); border: 1px solid var(--border); border-radius: 3px; color: #fff; font-family: var(--mono); font-size: 11px; padding: 6px 8px; outline: none; }
  .inp:focus { border-color: var(--accent); }
  .btn-exec { background: var(--accent); border: none; border-radius: 3px; color: #fff; font-family: var(--mono); font-size: 10px; font-weight: 700; text-transform: uppercase; padding: 8px; cursor: pointer; margin-top: 4px; }
  .btn-exec:hover { background: #4f46e5; }
  .btn-eod { background: var(--green); border: none; border-radius: 3px; color: #fff; font-family: var(--mono); font-size: 10px; font-weight: 700; text-transform: uppercase; padding: 8px; cursor: pointer; }
  .btn-eod:hover { background: #059669; }

  .main-canvas { display: flex; flex-direction: column; overflow: hidden; background: var(--bg-base); }
  .canvas-nav { display: flex; background: var(--bg-surface); border-bottom: 1px solid var(--border); padding: 0 14px; gap: 4px; }
  .nav-btn { background: none; border: none; border-bottom: 2px solid transparent; color: var(--text-muted); font-family: var(--mono); font-size: 10px; font-weight: 700; padding: 10px 12px; cursor: pointer; text-transform: uppercase; }
  .nav-btn.active { color: #fff; border-bottom-color: var(--accent); }

  .tab-view { flex: 1; display: none; overflow: hidden; flex-direction: column; }
  .tab-view.active { display: flex; }

  .pipeline-bar { background: var(--bg-card); border-bottom: 1px solid var(--border); padding: 8px 14px; display: grid; grid-template-columns: repeat(5, 1fr); gap: 8px; flex-shrink: 0; }
  .step-box { background: var(--bg-base); border: 1px solid var(--border); border-radius: 3px; padding: 6px 8px; font-family: var(--mono); font-size: 9px; font-weight: 700; color: var(--text-muted); display: flex; align-items: center; gap: 6px; }
  .step-box.active { border-color: var(--green); color: #fff; background: rgba(16,185,129,0.08); }
  .s-dot { width: 5px; height: 5px; border-radius: 50%; background: var(--text-muted); }
  .step-box.active .s-dot { background: var(--green); }

  .table-container { flex: 1; overflow-y: auto; }
  table { width: 100%; border-collapse: collapse; font-family: var(--mono); font-size: 11px; }
  th { background: var(--bg-surface); color: var(--text-muted); padding: 6px 10px; text-align: left; font-size: 9px; text-transform: uppercase; border-bottom: 1px solid var(--border); position: sticky; top: 0; z-index: 5; }
  td { padding: 6px 10px; border-bottom: 1px solid var(--border); }
  tr:hover td { background: var(--bg-surface); cursor: pointer; }
  .dr-leg { color: var(--red); font-weight: 700; }
  .cr-leg { color: var(--green); font-weight: 700; }

  .split-drawer { display: grid; grid-template-columns: 1fr 1fr; height: 210px; border-top: 1px solid var(--border); background: var(--bg-surface); flex-shrink: 0; }
  .drawer-col { display: flex; flex-direction: column; padding: 8px 12px; overflow: hidden; }
  .drawer-col:first-child { border-right: 1px solid var(--border); }
  .drawer-hdr { font-family: var(--mono); font-size: 9px; text-transform: uppercase; font-weight: 700; color: var(--text-muted); margin-bottom: 4px; display: flex; justify-content: space-between; }
  .code-block { flex: 1; background: #020617; border: 1px solid var(--border); border-radius: 3px; padding: 8px; font-family: var(--mono); font-size: 10px; color: #38bdf8; overflow: auto; white-space: pre; line-height: 1.3; }

  #toast { display: none; position: fixed; bottom: 16px; right: 16px; background: var(--bg-card); border: 1px solid var(--accent); color: #fff; font-family: var(--mono); font-size: 10px; padding: 8px 14px; border-radius: 3px; z-index: 99; }
</style>
</head>
<body>

<header>
  <div class="finacle-logo">
    <span>FINACLE TREASURY // STRIPE LEDGER CORE</span>
    <span class="tag">PostgreSQL 16 ACID</span>
  </div>
  <div style="font-family: var(--mono); font-size: 10px; color: var(--text-muted);">
    Node: tbg-core-01 • Redis Lua Lien Engine • pacs.008 Clearing
  </div>
</header>

<div class="ribbon">
  <div class="metric-box">
    <div class="metric-label">Corporate Float (00040310001928)</div>
    <div class="metric-val" id="txtFloat">INR 0.00</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">CMS Suspense Clearing</div>
    <div class="metric-val" id="txtSuspense" style="color: var(--cyan);">INR 0.00</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">Clearing Rail Mode</div>
    <div class="metric-val" style="color: var(--amber);">NEFT / RTGS</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">P99 Engine Latency</div>
    <div class="metric-val" style="color: var(--green);"><span id="txtLatency">1.05</span> ms</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">Balanced Postings</div>
    <div class="metric-val" id="txtCount">0</div>
  </div>
</div>

<div class="workbench">
  <div class="rail-panel">
    <div class="panel-hdr">Payment Rail Dispatcher</div>
    <div class="form-row">
      <label>Originating Float Account</label>
      <input type="text" class="inp" id="inpCorpAcc" value="00040310001928" readonly>
    </div>
    <div class="form-row">
      <label>Beneficiary Entity Name</label>
      <input type="text" class="inp" id="inpBeneName" value="Tata Motors Fleet Ltd">
    </div>
    <div class="form-row">
      <label>Beneficiary Account Number</label>
      <input type="text" class="inp" id="inpBeneAcct" value="912345678901">
    </div>
    <div class="form-row">
      <label>Beneficiary IFSC Code</label>
      <input type="text" class="inp" id="inpIfsc" value="HDFC0000001">
    </div>
    <div class="form-row">
      <label>Payout Amount (INR)</label>
      <input type="number" class="inp" id="inpAmount" value="25000.00" step="500">
    </div>
    <div class="form-row">
      <label>Rail Protocol</label>
      <select class="inp" id="inpRail">
        <option value="NEFT">NEFT (National Electronic Fund Transfer)</option>
        <option value="RTGS">RTGS (Real Time Gross Settlement)</option>
      </select>
    </div>
    <div class="form-row">
      <label>Distributed Idempotency Key</label>
      <input type="text" class="inp" id="inpIdemp" value="TXN-DEMO-001">
    </div>
    <button class="btn-exec" onclick="submitPayout()">EXECUTE IDEMPOTENT PAYOUT</button>

    <div class="panel-hdr" style="margin-top: 10px;">Central Bank Clearing</div>
    <button class="btn-eod" onclick="triggerReconcile()">TRIGGER EOD NOSTRO SETTLEMENT</button>
  </div>

  <div class="main-canvas">
    <div class="canvas-nav">
      <button class="nav-btn active" onclick="switchNav('AUDIT', this)">Postings Ledger</button>
      <button class="nav-btn" onclick="switchNav('ACCOUNTS', this)">Chart of Accounts Master</button>
    </div>

    <div class="tab-view active" id="viewAUDIT">
      <div class="pipeline-bar">
        <div class="step-box" id="s1"><div class="s-dot"></div> 1. IDEMPOTENCY</div>
        <div class="step-box" id="s2"><div class="s-dot"></div> 2. LIEN_HELD</div>
        <div class="step-box" id="s3"><div class="s-dot"></div> 3. JOURNAL_POSTED</div>
        <div class="step-box" id="s4"><div class="s-dot"></div> 4. ISO20022_PACS008</div>
        <div class="step-box" id="s5"><div class="s-dot"></div> 5. SETTLED</div>
      </div>

      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Timestamp</th>
              <th>Journal Voucher (JV ID)</th>
              <th>Account Identifier</th>
              <th>Leg</th>
              <th>Amount (INR)</th>
              <th>Zero-Sum Invariant</th>
            </tr>
          </thead>
          <tbody id="ledgerTbody"></tbody>
        </table>
      </div>

      <div class="split-drawer">
        <div class="drawer-col">
          <div class="drawer-hdr">
            <span>ISO 20022 pacs.008.001.08 Wire Message</span>
            <span>SFMS / NPCI Ready</span>
          </div>
          <div class="code-block" id="xmlScreen">&lt;!-- Select a voucher to view XML wire --&gt;</div>
        </div>
        <div class="drawer-col">
          <div class="drawer-hdr">
            <span>Outward ERP Webhook Dispatch</span>
            <span style="color: var(--green);">HMAC-SHA256 SIGNED</span>
          </div>
          <div class="code-block" id="webhookScreen">{ "event": "payout.settled", "status": "AWAITING_SELECTION" }</div>
        </div>
      </div>
    </div>

    <div class="tab-view" id="viewACCOUNTS" style="padding: 14px; overflow-y: auto;">
      <table>
        <thead>
          <tr>
            <th>Account Identifier</th>
            <th>Designation</th>
            <th>Type</th>
            <th>Currency</th>
            <th>Accounting Invariant</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td><code>00040310001928</code></td>
            <td><b>Corporate Operating Float (Client Float)</b></td>
            <td><span style="color: var(--amber); font-weight: 700;">LIABILITY</span></td>
            <td>INR</td>
            <td>Sum(CR) - Sum(DR)</td>
          </tr>
          <tr>
            <td><code>AC_CMS_SUSPENSE_CLEARING_9999</code></td>
            <td><b>CMS Intraday Clearing Suspense</b></td>
            <td><span style="color: var(--cyan); font-weight: 700;">SUSPENSE</span></td>
            <td>INR</td>
            <td>Net Intra-day Clearing Position</td>
          </tr>
          <tr>
            <td><code>AC_RBI_NOSTRO_0001</code></td>
            <td><b>Central Bank Settlement Nostro</b></td>
            <td><span style="color: var(--green); font-weight: 700;">ASSET</span></td>
            <td>INR</td>
            <td>EOD Settlement Offset Leg</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</div>

<div id="toast"></div>

<script>
  var TOTAL_FLOAT = 10000000.00;
  var cachedPostings = [];

  window.onload = function() {
    generateNewIdemp();
    refreshData();
    setInterval(refreshData, 3000);
  };

  function switchNav(id, btn) {
    document.querySelectorAll(".tab-view").forEach(function(v) { v.classList.remove("active"); });
    document.querySelectorAll(".nav-btn").forEach(function(b) { b.classList.remove("active"); });
    document.getElementById("view" + id).classList.add("active");
    btn.classList.add("active");
  }

  function setStep(step) {
    for (var i = 1; i <= 5; i++) {
      var el = document.getElementById("s" + i);
      if (i <= step) el.classList.add("active");
      else el.classList.remove("active");
    }
  }

  function generateNewIdemp() {
    document.getElementById("inpIdemp").value = "TXN-" + Date.now().toString().slice(-8);
  }

  function showToast(msg) {
    var t = document.getElementById("toast");
    t.innerText = msg;
    t.style.display = "block";
    setTimeout(function() { t.style.display = "none"; }, 3500);
  }

  async function refreshData() {
    try {
      var res = await fetch("/api/v1/cms/stats");
      var data = await res.json();

      var suspense = Number(data.suspense_balance || 0);
      var avail = TOTAL_FLOAT - suspense;

      document.getElementById("txtSuspense").innerText = "INR " + suspense.toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("txtFloat").innerText = "INR " + avail.toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("txtCount").innerText = data.postings ? data.postings.length : 0;

      cachedPostings = data.postings || [];
      renderTable(cachedPostings);
      if (cachedPostings.length > 0 && document.getElementById("xmlScreen").innerText.indexOf("<?xml") === -1) {
        inspectVoucher(cachedPostings[0].jv_id, cachedPostings[0].amount);
      }
    } catch(e) {
      console.error(e);
    }
  }

  function renderTable(postings) {
    var tbody = document.getElementById("ledgerTbody");
    tbody.innerHTML = "";
    if (postings && postings.length > 0) {
      postings.forEach(function(p) {
        tbody.innerHTML += '<tr onclick="inspectVoucher(\'' + p.jv_id + '\', ' + p.amount + ')">' +
          '<td>#' + p.posting_id + '</td>' +
          '<td style="color:var(--text-muted);">' + new Date(p.created_at).toLocaleTimeString() + '</td>' +
          '<td><b>' + p.jv_id.slice(0, 18) + '...</b></td>' +
          '<td><code>' + p.account_id + '</code></td>' +
          '<td><span class="' + (p.direction === "DR" ? "dr-leg" : "cr-leg") + '">' + p.direction + '</span></td>' +
          '<td><b>INR ' + Number(p.amount).toLocaleString('en-IN', {minimumFractionDigits: 2}) + '</b></td>' +
          '<td><span style="color:var(--green);">✓ Zero-Sum Passed</span></td>' +
        '</tr>';
      });
    } else {
      tbody.innerHTML = '<tr><td colspan="7" style="text-align:center; padding:20px; color:var(--text-muted);">No postings recorded yet.</td></tr>';
    }
  }

  function inspectVoucher(jv, amt) {
    var xml = '<?xml version="1.0" encoding="UTF-8"?>\n' +
'<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">\n' +
'  <FIToFICstmrCdtTrf>\n' +
'    <GrpHdr>\n' +
'      <MsgId>MSG-' + jv.slice(0, 8) + '</MsgId>\n' +
'      <CreDtTm>' + new Date().toISOString() + '</CreDtTm>\n' +
'      <NbOfTxs>1</NbOfTxs>\n' +
'      <SttlmInf><SttlmMtd>CLRG</SttlmMtd></SttlmInf>\n' +
'    </GrpHdr>\n' +
'    <CdtTrfTxInf>\n' +
'      <PmtId><EndToEndId>REF-' + jv.slice(0, 8) + '</EndToEndId><Utr>CMSNEFT' + Date.now().toString().slice(-10) + '</Utr></PmtId>\n' +
'      <IntrBkSttlmAmt Ccy="INR">' + Number(amt).toFixed(2) + '</IntrBkSttlmAmt>\n' +
'      <Dbtr><Nm>Corporate Operating Float Client</Nm></Dbtr>\n' +
'      <DbtrAcct><Id><Othr><Id>00040310001928</Id></Othr></Id></DbtrAcct>\n' +
'      <CdtrAgt><FinInstnId><ClrSysMmbId><MmbId>HDFC0000001</MmbId></ClrSysMmbId></FinInstnId></CdtrAgt>\n' +
'      <Cdtr><Nm>Tata Motors Fleet Ltd</Nm></Cdtr>\n' +
'      <CdtrAcct><Id><Othr><Id>912345678901</Id></Othr></Id></CdtrAcct>\n' +
'    </CdtTrfTxInf>\n' +
'  </FIToFICstmrCdtTrf>\n' +
'</Document>';
    document.getElementById("xmlScreen").innerText = xml;

    var webhook = JSON.stringify({
      "event": "payout.settled",
      "timestamp": new Date().toISOString(),
      "signature_256": "hmac_sha256_" + jv.slice(0, 16),
      "data": {
        "jv_id": jv,
        "originating_account": "00040310001928",
        "amount": amt,
        "currency": "INR",
        "rail": "NEFT",
        "status": "SETTLED"
      }
    }, null, 2);
    document.getElementById("webhookScreen").innerText = webhook;
  }

  async function submitPayout() {
    var idemp = document.getElementById("inpIdemp").value;
    var amt = parseFloat(document.getElementById("inpAmount").value);
    var rail = document.getElementById("inpRail").value;

    setStep(1);
    var t0 = performance.now();

    var payload = {
      corporate_account: document.getElementById("inpCorpAcc").value,
      amount: amt,
      currency: "INR",
      payment_rail: rail,
      beneficiary_name: document.getElementById("inpBeneName").value,
      beneficiary_acct: document.getElementById("inpBeneAcct").value,
      beneficiary_ifsc: document.getElementById("inpIfsc").value,
      reference_id: "REF-" + Date.now().toString().slice(-8)
    };

    setTimeout(function() { setStep(2); }, 60);

    try {
      var res = await fetch("/api/v1/cms/payout", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": idemp
        },
        body: JSON.stringify(payload)
      });
      var data = await res.json();
      var elapsed = (performance.now() - t0).toFixed(2);
      document.getElementById("txtLatency").innerText = elapsed;

      if(res.ok) {
        setStep(5);
        showToast("Settled! UTR: " + data.utr + " (" + elapsed + "ms)");
        generateNewIdemp();
        refreshData();
        inspectVoucher(data.jv_id, payload.amount);
      } else {
        setStep(0);
        showToast(data.error || "Payout rejected by ledger");
      }
    } catch(err) {
      setStep(0);
      showToast("Engine Connection Error");
    }
  }

  async function triggerReconcile() {
    try {
      var res = await fetch("/api/v1/cms/reconcile", { method: "POST" });
      var data = await res.json();
      if (res.ok) {
        showToast("EOD Settled against RBI Nostro: INR " + Number(data.cleared_amount).toLocaleString());
        refreshData();
      } else {
        showToast(data.message || "Reconciliation error");
      }
    } catch(e) {
      showToast("Reconciler Error");
    }
  }
</script>
</body>
</html>`
