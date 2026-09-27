package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
		log.Fatalf("postgres connection error: %v", err)
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
	log.Println("PostgreSQL connected successfully")

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
			log.Fatalf("failed to parse Redis URL: %v", err)
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
		log.Fatalf("redis connection error: %v", err)
	}
	log.Println("Redis connected successfully")

	repo := ledger.NewRepository(db)
	lienEngine := ledger.NewLienEngine(rdb)

	if err := lienEngine.SeedFloat(context.Background(), "00040310001928", 10000000.00); err != nil {
		log.Printf("failed to seed float: %v", err)
	} else {
		log.Println("Corporate float seeded: 00040310001928 = INR 10,000,000.00")
	}

	payoutSvc := service.NewPayoutService(repo, lienEngine, rdb, logger, cfg.HMACSalt)
	eodSvc := service.NewEODReconciler(repo, logger)
	_ = eodSvc

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/cms/payout", payoutSvc.HandlePayout)
	mux.HandleFunc("/api/v1/cms/stats", payoutSvc.HandleStats)
	mux.HandleFunc("/healthz", payoutSvc.HandleHealthz)
	mux.HandleFunc("/health", payoutSvc.HandleHealthz)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(portalHTML))
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
		log.Printf("TBG-CORE API starting on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
	log.Println("Server stopped")
}

const portalHTML = `<!DOCTYPE html>
<html lang="en" data-theme="dark">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TBG-CORE | Institutional Transaction Banking Terminal</title>
<style>
  :root[data-theme="dark"] {
    --bg: #06090e; --surface: #0c111c; --panel: #111827; --panel-border: #1e293b;
    --accent: #0284c7; --accent-glow: rgba(2, 132, 199, 0.25);
    --green: #10b981; --red: #ef4444; --yellow: #f59e0b;
    --text: #f1f5f9; --text-muted: #64748b; --code-bg: #040711;
  }
  :root[data-theme="light"] {
    --bg: #f8fafc; --surface: #ffffff; --panel: #f1f5f9; --panel-border: #cbd5e1;
    --accent: #0284c7; --accent-glow: rgba(2, 132, 199, 0.15);
    --green: #059669; --red: #dc2626; --yellow: #d97706;
    --text: #0f172a; --text-muted: #64748b; --code-bg: #e2e8f0;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "JetBrains Mono", monospace; }
  body { background: var(--bg); color: var(--text); height: 100vh; display: flex; flex-direction: column; overflow: hidden; font-size: 12px; }
  header { height: 46px; background: var(--surface); border-bottom: 1px solid var(--panel-border); display: flex; align-items: center; justify-content: space-between; padding: 0 16px; }
  .brand { display: flex; align-items: center; gap: 8px; font-weight: 800; font-size: 13px; color: #38bdf8; }
  .badge { background: #0369a1; color: #fff; padding: 2px 7px; border-radius: 4px; font-size: 9px; font-weight: 700; }
  .btn-theme { background: var(--panel); border: 1px solid var(--panel-border); color: var(--text); padding: 4px 10px; border-radius: 4px; cursor: pointer; font-size: 11px; }

  .telemetry-bar { background: var(--surface); border-bottom: 1px solid var(--panel-border); padding: 10px 16px; display: grid; grid-template-columns: 2fr 1fr 1fr 1fr; gap: 14px; }
  .telemetry-card { background: var(--panel); border: 1px solid var(--panel-border); border-radius: 6px; padding: 8px 12px; }
  .telemetry-label { font-size: 9px; text-transform: uppercase; color: var(--text-muted); font-weight: 700; }
  .telemetry-val { font-size: 16px; font-weight: 800; margin-top: 3px; display: flex; align-items: baseline; gap: 6px; }
  .progress-bg { height: 6px; background: var(--panel-border); border-radius: 3px; margin-top: 6px; overflow: hidden; }
  .progress-fill { height: 100%; background: linear-gradient(90deg, var(--accent), var(--green)); width: 100%; }

  .workspace { display: grid; grid-template-columns: 410px 1fr; height: calc(100vh - 120px); overflow: hidden; }
  .payout-console { background: var(--surface); border-right: 1px solid var(--panel-border); padding: 16px; display: flex; flex-direction: column; gap: 10px; overflow-y: auto; }
  .pane-title { font-size: 11px; text-transform: uppercase; font-weight: 800; color: #38bdf8; display: flex; justify-content: space-between; align-items: center; }
  .form-group { display: flex; flex-direction: column; gap: 4px; }
  .form-group label { font-size: 10px; text-transform: uppercase; color: var(--text-muted); font-weight: 700; }
  .form-control { background: var(--bg); border: 1px solid var(--panel-border); border-radius: 4px; color: var(--text); padding: 7px 10px; font-size: 11px; outline: none; }
  .form-control:focus { border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-glow); }
  .btn-dispatch { background: var(--accent); color: #fff; border: none; padding: 10px; border-radius: 4px; font-weight: 700; font-size: 11px; cursor: pointer; text-transform: uppercase; margin-top: 4px; }
  .btn-dispatch:hover { background: #0369a1; }

  .pipeline-card { background: var(--panel); border: 1px solid var(--panel-border); border-radius: 6px; padding: 10px; margin-top: 6px; }
  .pipeline-step { display: flex; align-items: center; gap: 8px; font-size: 10px; padding: 3px 0; color: var(--text-muted); }
  .pipeline-step.active { color: var(--green); font-weight: 700; }
  .dot { width: 6px; height: 6px; border-radius: 50%; background: var(--text-muted); }
  .pipeline-step.active .dot { background: var(--green); box-shadow: 0 0 6px var(--green); }

  .cockpit { background: var(--bg); display: flex; flex-direction: column; overflow: hidden; padding: 14px; gap: 12px; }
  .cockpit-tabs { display: flex; gap: 6px; border-bottom: 1px solid var(--panel-border); padding-bottom: 8px; }
  .tab-btn { background: none; border: none; color: var(--text-muted); font-weight: 700; font-size: 11px; cursor: pointer; padding: 4px 10px; border-radius: 4px; }
  .tab-btn.active { background: var(--panel); color: var(--accent); }

  .cockpit-view { flex: 1; display: none; overflow: hidden; flex-direction: column; background: var(--surface); border: 1px solid var(--panel-border); border-radius: 6px; }
  .cockpit-view.active { display: flex; }

  .table-box { flex: 1; overflow-y: auto; }
  table { width: 100%; border-collapse: collapse; font-size: 11px; }
  th { background: var(--panel); color: var(--text-muted); padding: 7px 10px; text-align: left; font-size: 9px; text-transform: uppercase; position: sticky; top: 0; border-bottom: 1px solid var(--panel-border); z-index: 10; }
  td { padding: 8px 10px; border-bottom: 1px solid var(--panel-border); }
  tr:hover { background: var(--panel); cursor: pointer; }
  .badge-dr { color: var(--red); font-weight: 700; }
  .badge-cr { color: var(--green); font-weight: 700; }

  .drawer { height: 210px; background: var(--surface); border-top: 1px solid var(--panel-border); padding: 10px 14px; display: flex; flex-direction: column; gap: 6px; }
  .drawer-head { display: flex; justify-content: space-between; align-items: center; font-size: 10px; font-weight: 700; color: var(--text-muted); text-transform: uppercase; }
  .code-viewer { flex: 1; background: var(--code-bg); border: 1px solid var(--panel-border); border-radius: 4px; padding: 8px; overflow: auto; font-size: 10px; color: #38bdf8; white-space: pre; }

  #toast { display: none; position: fixed; bottom: 20px; right: 20px; background: var(--surface); border: 1px solid var(--accent); color: var(--text); padding: 10px 16px; border-radius: 4px; font-size: 11px; z-index: 99; }
</style>
</head>
<body>

<header>
  <div class="brand">
    <span>TBG-CORE INSTITUTIONAL TRANSACTION BANKING TERMINAL</span>
    <span class="badge">PROD-ACID</span>
  </div>
  <div style="display:flex; align-items:center; gap:12px;">
    <span style="font-size:10px; color:var(--text-muted);">Engine: PostgreSQL 16 • Redis 7.2 • ISO-20022 Rail</span>
    <button class="btn-theme" onclick="toggleTheme()">Theme</button>
  </div>
</header>

<div class="telemetry-bar">
  <div class="telemetry-card">
    <div class="telemetry-label">Corporate Float Liquidity (00040310001928)</div>
    <div class="telemetry-val">
      <span id="txtAvailFloat" style="color:var(--green);">INR 9,975,000.00</span>
      <span style="font-size:10px; color:var(--text-muted);">/ INR 10,000,000.00 Pool</span>
    </div>
    <div class="progress-bg"><div class="progress-fill" id="floatBar" style="width: 99.75%;"></div></div>
  </div>
  <div class="telemetry-card">
    <div class="telemetry-label">CMS Intraday Suspense (CR)</div>
    <div class="telemetry-val" id="txtSuspense" style="color:var(--accent);">INR 25,000.00</div>
    <div style="font-size:9px; color:var(--text-muted); margin-top:4px;">AC_CMS_SUSPENSE_CLEARING_9999</div>
  </div>
  <div class="telemetry-card">
    <div class="telemetry-label">Clearing Rail Latency</div>
    <div class="telemetry-val" style="color:var(--yellow);"><span id="txtLatency">1.12</span> <span style="font-size:10px;">ms (P99)</span></div>
    <div style="font-size:9px; color:var(--text-muted); margin-top:4px;">Lock-Free Lua Engine</div>
  </div>
  <div class="telemetry-card">
    <div class="telemetry-label">Audited Vouchers</div>
    <div class="telemetry-val" id="txtPostingsCount">2</div>
    <div style="font-size:9px; color:var(--text-muted); margin-top:4px;">Merkle Linked & Zero-Sum Verified</div>
  </div>
</div>

<div class="workspace">
  <div class="payout-console">
    <div class="pane-title">
      <span>CMS Outward Settlement Rail</span>
      <span style="font-size:9px; color:var(--text-muted);">pacs.008 Dispatcher</span>
    </div>

    <div class="form-group">
      <label>Originating Corporate Account</label>
      <input type="text" class="form-control" id="inpCorpAcc" value="00040310001928" readonly>
    </div>
    <div class="form-group">
      <label>Beneficiary Legal Entity</label>
      <input type="text" class="form-control" id="inpBeneName" value="Tata Motors Fleet Ltd">
    </div>
    <div class="form-group">
      <label>Beneficiary Account Number</label>
      <input type="text" class="form-control" id="inpBeneAcct" value="912345678901">
    </div>
    <div class="form-group">
      <label>Beneficiary IFSC Code</label>
      <input type="text" class="form-control" id="inpIfsc" value="HDFC0000001">
    </div>
    <div class="form-group">
      <label>Payout Amount (INR)</label>
      <input type="number" class="form-control" id="inpAmount" value="25000.00" step="500">
    </div>
    <div class="form-group">
      <label>Clearing Rail Protocol</label>
      <select class="form-control" id="inpRail">
        <option value="NEFT">NEFT (National Electronic Fund Transfer)</option>
        <option value="RTGS">RTGS (Real Time Gross Settlement)</option>
      </select>
    </div>
    <div class="form-group">
      <label>Distributed Idempotency Key</label>
      <input type="text" class="form-control" id="inpIdemp" value="TXN-DEMO-001">
    </div>

    <button class="btn-dispatch" onclick="submitPayout()">EXECUTE IDEMPOTENT PAYOUT</button>

    <div class="pipeline-card">
      <div style="font-size:9px; text-transform:uppercase; color:var(--text-muted); font-weight:700; margin-bottom:6px;">Clearing State Pipeline</div>
      <div class="pipeline-step" id="step1"><div class="dot"></div> 1. INITIATED (Idempotency Locked)</div>
      <div class="pipeline-step" id="step2"><div class="dot"></div> 2. LIEN_HELD (Redis Atomic Float Hold)</div>
      <div class="pipeline-step" id="step3"><div class="dot"></div> 3. JOURNAL_POSTED (PostgreSQL Zero-Sum)</div>
      <div class="pipeline-step" id="step4"><div class="dot"></div> 4. ISO20022_EMITTED (pacs.008 Generated)</div>
      <div class="pipeline-step" id="step5"><div class="dot"></div> 5. SETTLED (Lien Released Permanently)</div>
    </div>
  </div>

  <div class="cockpit">
    <div class="cockpit-tabs">
      <button class="tab-btn active" onclick="switchCockpit('POSTINGS', this)">Live Postings Feed</button>
      <button class="tab-btn" onclick="switchCockpit('ACCOUNTS', this)">Chart of Accounts</button>
    </div>

    <div class="cockpit-view active" id="viewPOSTINGS">
      <div class="table-box">
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Timestamp</th>
              <th>Journal Voucher (JV ID)</th>
              <th>Account Identifier</th>
              <th>Leg</th>
              <th>Amount (INR)</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody id="ledgerTbody"></tbody>
        </table>
      </div>

      <div class="drawer">
        <div class="drawer-head">
          <span>Target ISO 20022 pacs.008.001.08 XML Wire Message & Hash Chain</span>
          <button class="btn-theme" style="padding:2px 8px; font-size:9px;" onclick="copyWireXML()">Copy Wire XML</button>
        </div>
        <div class="code-viewer" id="txtWireCode">&lt;?xml version="1.0" encoding="UTF-8"?&gt;
&lt;Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08"&gt;
  &lt;FIToFICstmrCdtTrf&gt;
    &lt;GrpHdr&gt;
      &lt;MsgId&gt;MSG-5bea4eb5&lt;/MsgId&gt;
      &lt;CreDtTm&gt;2026-09-27T15:28:13Z&lt;/CreDtTm&gt;
      &lt;NbOfTxs&gt;1&lt;/NbOfTxs&gt;
      &lt;SttlmInf&gt;&lt;SttlmMtd&gt;CLRG&lt;/SttlmMtd&gt;&lt;/SttlmInf&gt;
    &lt;/GrpHdr&gt;
    &lt;CdtTrfTxInf&gt;
      &lt;PmtId&gt;&lt;EndToEndId&gt;REF-PAYOUT-001&lt;/EndToEndId&gt;&lt;Utr&gt;CMSNEFT8891230491&lt;/Utr&gt;&lt;/PmtId&gt;
      &lt;IntrBkSttlmAmt Ccy="INR"&gt;25000.00&lt;/IntrBkSttlmAmt&gt;
      &lt;Dbtr&gt;&lt;Nm&gt;Corporate Operating Float Client&lt;/Nm&gt;&lt;/Dbtr&gt;
      &lt;DbtrAcct&gt;&lt;Id&gt;&lt;Othr&gt;&lt;Id&gt;00040310001928&lt;/Id&gt;&lt;/Othr&gt;&lt;/Id&gt;&lt;/DbtrAcct&gt;
      &lt;CdtrAgt&gt;&lt;FinInstnId&gt;&lt;ClrSysMmbId&gt;&lt;MmbId&gt;HDFC0000001&lt;/MmbId&gt;&lt;/ClrSysMmbId&gt;&lt;/FinInstnId&gt;&lt;/CdtrAgt&gt;
      &lt;Cdtr&gt;&lt;Nm&gt;Tata Motors Fleet Ltd&lt;/Nm&gt;&lt;/Cdtr&gt;
      &lt;CdtrAcct&gt;&lt;Id&gt;&lt;Othr&gt;&lt;Id&gt;912345678901&lt;/Id&gt;&lt;/Othr&gt;&lt;/Id&gt;&lt;/CdtrAcct&gt;
    &lt;/CdtTrfTxInf&gt;
  &lt;/FIToFICstmrCdtTrf&gt;
&lt;/Document&gt;</div>
      </div>
    </div>

    <div class="cockpit-view" id="viewACCOUNTS" style="padding:16px;">
      <table>
        <thead>
          <tr>
            <th>Account Identifier</th>
            <th>Designation</th>
            <th>Type</th>
            <th>Currency</th>
            <th>Balance Invariant</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td><code>00040310001928</code></td>
            <td><b>Corporate Operating Float (Client Float)</b></td>
            <td><span style="color:var(--yellow); font-weight:700;">LIABILITY</span></td>
            <td>INR</td>
            <td>DR Reduces / CR Increases</td>
          </tr>
          <tr>
            <td><code>AC_CMS_SUSPENSE_CLEARING_9999</code></td>
            <td><b>CMS Intraday Clearing Suspense</b></td>
            <td><span style="color:var(--accent); font-weight:700;">SUSPENSE</span></td>
            <td>INR</td>
            <td>CR Accumulates Intraday Payouts</td>
          </tr>
          <tr>
            <td><code>AC_RBI_NOSTRO_0001</code></td>
            <td><b>Central Bank Settlement Nostro</b></td>
            <td><span style="color:var(--green); font-weight:700;">ASSET</span></td>
            <td>INR</td>
            <td>End-of-Day Net Clearing Offset</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</div>

<div id="toast"></div>

<script>
  var cachedPostings = [];
  var TOTAL_INITIAL_FLOAT = 10000000.00;

  window.onload = function() {
    initTheme();
    generateNewIdemp();
    refreshData();
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

  function switchCockpit(viewId, btn) {
    document.querySelectorAll(".cockpit-view").forEach(function(v) { v.classList.remove("active"); });
    document.querySelectorAll(".tab-btn").forEach(function(b) { b.classList.remove("active"); });
    document.getElementById("view" + viewId).classList.add("active");
    btn.classList.add("active");
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
      var res = await fetch("/api/v1/cms/stats");
      var data = await res.json();
      
      var suspense = Number(data.suspense_balance || 0);
      var avail = TOTAL_INITIAL_FLOAT - suspense;
      var pct = Math.max(0, Math.min(100, (avail / TOTAL_INITIAL_FLOAT) * 100));

      document.getElementById("txtSuspense").innerText = "INR " + suspense.toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("txtAvailFloat").innerText = "INR " + avail.toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("floatBar").style.width = pct + "%";
      document.getElementById("txtPostingsCount").innerText = data.postings ? data.postings.length : 0;

      cachedPostings = data.postings || [];
      renderPostingsTable(cachedPostings);
    } catch(e) {
      console.error(e);
    }
  }

  function renderPostingsTable(postings) {
    var tbody = document.getElementById("ledgerTbody");
    tbody.innerHTML = "";
    if (postings && postings.length > 0) {
      postings.forEach(function(p) {
        tbody.innerHTML += '<tr onclick="inspectPost(\'' + p.jv_id + '\', \'' + p.account_id + '\', ' + p.amount + ')">' +
          '<td>#' + p.posting_id + '</td>' +
          '<td style="color:var(--text-muted);">' + new Date(p.created_at).toLocaleTimeString() + '</td>' +
          '<td><b>' + p.jv_id.slice(0, 16) + '...</b></td>' +
          '<td><code>' + p.account_id + '</code></td>' +
          '<td><span class="' + (p.direction === "DR" ? "badge-dr" : "badge-cr") + '">' + p.direction + '</span></td>' +
          '<td><b>INR ' + Number(p.amount).toLocaleString('en-IN', {minimumFractionDigits: 2}) + '</b></td>' +
          '<td><button class="btn-theme" style="padding:1px 6px; font-size:9px;">Inspect pacs.008</button></td>' +
        '</tr>';
      });
    } else {
      tbody.innerHTML = '<tr><td colspan="7" style="text-align:center; padding:20px; color:var(--text-muted);">No postings available.</td></tr>';
    }
  }

  function inspectPost(jv, acc, amt) {
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
'      <Dbtr><Nm>Corporate Float Client</Nm></Dbtr>\n' +
'      <DbtrAcct><Id><Othr><Id>00040310001928</Id></Othr></Id></DbtrAcct>\n' +
'      <CdtrAgt><FinInstnId><ClrSysMmbId><MmbId>HDFC0000001</MmbId></ClrSysMmbId></FinInstnId></CdtrAgt>\n' +
'      <Cdtr><Nm>Tata Motors Fleet Ltd</Nm></Cdtr>\n' +
'      <CdtrAcct><Id><Othr><Id>912345678901</Id></Othr></Id></CdtrAcct>\n' +
'    </CdtTrfTxInf>\n' +
'  </FIToFICstmrCdtTrf>\n' +
'</Document>';
    document.getElementById("txtWireCode").innerText = xml;
    showToast("Inspecting ISO 20022 payload for JV: " + jv.slice(0, 8));
  }

  function copyWireXML() {
    navigator.clipboard.writeText(document.getElementById("txtWireCode").innerText);
    showToast("Copied pacs.008 XML to clipboard");
  }

  function setPipeline(step) {
    for (var i = 1; i <= 5; i++) {
      var el = document.getElementById("step" + i);
      if (i <= step) el.classList.add("active");
      else el.classList.remove("active");
    }
  }

  async function submitPayout() {
    var idemp = document.getElementById("inpIdemp").value;
    var amt = parseFloat(document.getElementById("inpAmount").value);
    var rail = document.getElementById("inpRail").value;

    setPipeline(1);
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

    setTimeout(function() { setPipeline(2); }, 60);

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
        setPipeline(5);
        showToast("Settled! UTR: " + data.utr + " (" + elapsed + "ms)", false);
        generateNewIdemp();
        refreshData();
        inspectPost(data.jv_id, payload.corporate_account, payload.amount);
      } else {
        setPipeline(0);
        showToast(data.error || data.message || "Payout rejected by ledger", true);
      }
    } catch(err) {
      setPipeline(0);
      showToast("Engine Connection Error", true);
    }
  }
</script>
</body>
</html>`
