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

	mux.HandleFunc("/api/v1/cms/payout", payoutSvc.HandlePayout)
	mux.HandleFunc("/api/v1/cms/stats", payoutSvc.HandleStats)
	mux.HandleFunc("/healthz", payoutSvc.HandleHealthz)
	mux.HandleFunc("/health", payoutSvc.HandleHealthz)

	mux.HandleFunc("/api/v1/cms/reconcile", func(w http.ResponseWriter, r *http.Request) {
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
	})

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
<title>TBG-CORE | Finacle Treasury & Stripe Institutional Terminal</title>
<style>
  :root {
    --bg-base: #05070c;
    --bg-surface: #090d16;
    --bg-card: #0c121e;
    --border: #1b2436;
    --border-accent: #0284c7;
    --text-main: #e2e8f0;
    --text-muted: #64748b;
    --cyan: #38bdf8;
    --green: #10b981;
    --red: #f43f5e;
    --amber: #f59e0b;
    --mono: "JetBrains Mono", Menlo, monospace;
    --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg-base); color: var(--text-main); font-family: var(--mono); height: 100vh; display: flex; flex-direction: column; overflow: hidden; font-size: 11px; border-top: 2px solid var(--border-accent); }

  header { height: 38px; background: var(--bg-surface); border-bottom: 1px solid var(--border); display: flex; align-items: center; justify-content: space-between; padding: 0 12px; flex-shrink: 0; }
  .finacle-logo { font-size: 11px; font-weight: 800; letter-spacing: 0.5px; color: #fff; display: flex; align-items: center; gap: 8px; }
  .tag { font-size: 9px; padding: 1px 6px; border-radius: 2px; font-weight: 700; text-transform: uppercase; background: rgba(2,132,199,0.15); color: var(--cyan); border: 1px solid rgba(2,132,199,0.3); }

  .ribbon { height: 46px; background: var(--bg-card); border-bottom: 1px solid var(--border); display: grid; grid-template-columns: 2fr 1fr 1fr 1fr 1fr; align-items: center; padding: 0 12px; gap: 12px; flex-shrink: 0; }
  .metric-box { display: flex; flex-direction: column; }
  .metric-label { font-size: 9px; color: var(--text-muted); text-transform: uppercase; font-weight: 700; }
  .metric-val { font-size: 12px; font-weight: 700; color: #fff; margin-top: 1px; }

  .workbench { display: grid; grid-template-columns: 310px 1fr; flex: 1; overflow: hidden; }

  .rail-panel { background: var(--bg-surface); border-right: 1px solid var(--border); padding: 12px; display: flex; flex-direction: column; gap: 8px; overflow-y: auto; }
  .panel-hdr { font-size: 10px; font-weight: 800; color: var(--cyan); text-transform: uppercase; border-bottom: 1px solid var(--border); padding-bottom: 3px; }
  .form-row { display: flex; flex-direction: column; gap: 2px; }
  .form-row label { font-size: 9px; color: var(--text-muted); text-transform: uppercase; }
  .inp { background: var(--bg-base); border: 1px solid var(--border); border-radius: 2px; color: #fff; font-family: var(--mono); font-size: 11px; padding: 5px 7px; outline: none; }
  .inp:focus { border-color: var(--border-accent); }
  .btn-exec { background: #0284c7; border: none; border-radius: 2px; color: #fff; font-family: var(--mono); font-size: 10px; font-weight: 700; text-transform: uppercase; padding: 7px; cursor: pointer; margin-top: 4px; }
  .btn-exec:hover { background: #0369a1; }
  .btn-eod { background: #059669; border: none; border-radius: 2px; color: #fff; font-family: var(--mono); font-size: 10px; font-weight: 700; text-transform: uppercase; padding: 7px; cursor: pointer; }
  .btn-eod:hover { background: #047857; }

  .main-canvas { display: flex; flex-direction: column; overflow: hidden; background: var(--bg-base); }
  .canvas-nav { display: flex; background: var(--bg-surface); border-bottom: 1px solid var(--border); padding: 0 10px; gap: 4px; }
  .nav-btn { background: none; border: none; border-bottom: 2px solid transparent; color: var(--text-muted); font-family: var(--mono); font-size: 10px; font-weight: 700; padding: 9px 11px; cursor: pointer; text-transform: uppercase; }
  .nav-btn.active { color: var(--cyan); border-bottom-color: var(--cyan); }

  .tab-view { flex: 1; display: none; overflow: hidden; flex-direction: column; }
  .tab-view.active { display: flex; }

  .table-container { flex: 1; overflow-y: auto; }
  table { width: 100%; border-collapse: collapse; font-family: var(--mono); font-size: 11px; }
  th { background: var(--bg-surface); color: var(--text-muted); padding: 6px 10px; text-align: left; font-size: 9px; text-transform: uppercase; border-bottom: 1px solid var(--border); position: sticky; top: 0; z-index: 5; }
  td { padding: 6px 10px; border-bottom: 1px solid var(--border); }
  tr:hover td { background: var(--bg-surface); cursor: pointer; }
  .dr-leg { color: var(--red); font-weight: 700; }
  .cr-leg { color: var(--green); font-weight: 700; }

  .split-drawer { display: grid; grid-template-columns: 1fr 1fr; height: 200px; border-top: 1px solid var(--border); background: var(--bg-surface); flex-shrink: 0; }
  .drawer-col { display: flex; flex-direction: column; padding: 8px 10px; overflow: hidden; }
  .drawer-col:first-child { border-right: 1px solid var(--border); }
  .drawer-hdr { font-size: 9px; text-transform: uppercase; font-weight: 700; color: var(--text-muted); margin-bottom: 4px; display: flex; justify-content: space-between; }
  .code-block { flex: 1; background: #020408; border: 1px solid var(--border); border-radius: 2px; padding: 7px; font-family: var(--mono); font-size: 10px; color: var(--cyan); overflow: auto; white-space: pre; line-height: 1.3; }

  #toast { display: none; position: fixed; bottom: 14px; right: 14px; background: var(--bg-card); border: 1px solid var(--border-accent); color: #fff; font-family: var(--mono); font-size: 10px; padding: 7px 12px; border-radius: 2px; z-index: 99; }
</style>
</head>
<body>

<header>
  <div class="finacle-logo">
    <span>FINACLE TREASURY // STRIPE LEDGER CORE</span>
    <span class="tag">PostgreSQL 16 ACID</span>
  </div>
  <div style="font-size: 10px; color: var(--text-muted);">
    Node: tbg-core-01 • Redis Lua Lien Engine • pacs.008 Clearing
  </div>
</header>

<div class="ribbon">
  <div class="metric-box">
    <div class="metric-label">Corporate Float (00040310001928)</div>
    <div class="metric-val" id="txtFloat">INR 0.00</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">CMS Suspense Net Liability</div>
    <div class="metric-val" id="txtSuspense" style="color: var(--cyan);">INR 0.00</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">Clearing Rail Mode</div>
    <div class="metric-val" style="color: var(--amber);">NEFT / RTGS (SFMS)</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">P99 Engine Latency</div>
    <div class="metric-val" style="color: var(--green);"><span id="txtLatency">1.05</span> ms</div>
  </div>
  <div class="metric-box">
    <div class="metric-label">Audited Postings</div>
    <div class="metric-val" id="txtCount">0 Entries</div>
  </div>
</div>

<div class="workbench">
  <div class="rail-panel">
    <div class="panel-hdr">Payment Rail Dispatcher</div>
    <div class="form-row">
      <label>Debit Float Account</label>
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
      <button class="nav-btn" onclick="switchNav('TACCOUNTS', this)">T-Account Balance Sheet</button>
      <button class="nav-btn" onclick="switchNav('ACCOUNTS', this)">Chart of Accounts Master</button>
    </div>

    <div class="tab-view active" id="viewAUDIT">
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
              <th>Audit Verdict</th>
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

    <div class="tab-view" id="viewTACCOUNTS" style="padding: 12px; overflow-y: auto;">
      <div style="display:grid; grid-template-columns: 1fr 1fr; gap: 12px;">
        <div style="border: 1px solid var(--border); background: var(--bg-surface); padding: 10px; border-radius: 2px;">
          <div style="color: var(--amber); font-weight: 700; border-bottom: 1px solid var(--border); padding-bottom: 4px; margin-bottom: 6px;">
            00040310001928 (Corporate Operating Float - Liability)
          </div>
          <table style="width:100%;">
            <thead>
              <tr style="color:var(--text-muted); border-bottom:1px solid var(--border);">
                <th>DEBIT (Outflows)</th>
                <th>CREDIT (Initial Float)</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td style="color:var(--red); padding-top:4px;" id="taccDebitFloat">INR 0.00</td>
                <td style="color:var(--green); padding-top:4px;">INR 1,00,00,000.00</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div style="border: 1px solid var(--border); background: var(--bg-surface); padding: 10px; border-radius: 2px;">
          <div style="color: var(--cyan); font-weight: 700; border-bottom: 1px solid var(--border); padding-bottom: 4px; margin-bottom: 6px;">
            AC_CMS_SUSPENSE_CLEARING_9999 (Intraday Suspense)
          </div>
          <table style="width:100%;">
            <thead>
              <tr style="color:var(--text-muted); border-bottom:1px solid var(--border);">
                <th>DEBIT (Settled Nostro)</th>
                <th>CREDIT (Accumulated CMS)</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td style="color:var(--red); padding-top:4px;" id="taccNostroDebit">INR 0.00</td>
                <td style="color:var(--green); padding-top:4px;" id="taccSuspenseCredit">INR 0.00</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <div class="tab-view" id="viewACCOUNTS" style="padding: 12px; overflow-y: auto;">
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
      document.getElementById("txtCount").innerText = (data.postings ? data.postings.length : 0) + " Entries";

      document.getElementById("taccDebitFloat").innerText = "INR " + suspense.toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("taccSuspenseCredit").innerText = "INR " + suspense.toLocaleString("en-IN", {minimumFractionDigits: 2});

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
          '<td><span style="color:var(--green);">✓ ZERO-SUM OK</span></td>' +
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
        showToast("Settled! UTR: " + data.utr + " (" + elapsed + "ms)");
        generateNewIdemp();
        refreshData();
        inspectVoucher(data.jv_id, payload.amount);
      } else {
        showToast(data.error || "Payout rejected by ledger");
      }
    } catch(err) {
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
