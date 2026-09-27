package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"html/template"
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

var portalTmpl = template.Must(template.New("portal").Parse(`<!DOCTYPE html>
<html lang="en" data-theme="dark">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>TBG-CORE | Transaction Banking Portal</title>
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
  * { box-sizing: border-box; margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "JetBrains Mono", "Segoe UI", monospace; transition: background 0.15s, color 0.15s; }
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
    <span class="badge">PROD-ACID</span>
  </div>
  <div class="nav-right">
    <span style="font-size:11px; color:var(--muted);"><span class="live-dot"></span>PostgreSQL 16 • Redis 7.2 • ISO-20022</span>
    <button class="theme-toggle" onclick="toggleTheme()">☀️ / 🌙 Theme</button>
  </div>
</header>

<div class="tab-strip">
  <button class="tab-btn active" onclick="switchView('WORKSTATION', this)">Payouts & Live Ledger</button>
  <button class="tab-btn" onclick="switchView('ACCOUNTS', this)">Ledger Accounts Position</button>
  <button class="tab-btn" onclick="switchView('TELEMETRY', this)">System Health & Nodes</button>
</div>

<div class="ribbon">
  <div class="card">
    <div class="card-label">Corporate Float Account</div>
    <div class="card-val" id="valCorpAcc">00040310001928</div>
  </div>
  <div class="card">
    <div class="card-label">CMS Suspense Balance (CR)</div>
    <div class="card-val" id="valSuspense">₹0.00</div>
  </div>
  <div class="card">
    <div class="card-label">Settlement Clearing Rail</div>
    <div class="card-val" style="color:var(--accent);">NEFT / RTGS</div>
  </div>
  <div class="card">
    <div class="card-label">Total Ledger Postings</div>
    <div class="card-val" id="valPostingsCount">0</div>
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
        <label>Debit Corporate Account:</label>
        <input type="text" id="inpCorpAcc" value="00040310001928" readonly>
      </div>
      <div class="form-group">
        <label>Beneficiary Legal Name:</label>
        <input type="text" id="inpBeneName" value="Tata Motors Fleet Ltd">
      </div>
      <div class="form-group">
        <label>Beneficiary Account Number:</label>
        <input type="text" id="inpBeneAcct" value="912345678901">
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
        <label>Payment Rail:</label>
        <select id="inpRail">
          <option value="NEFT">NEFT (National Electronic Fund Transfer)</option>
          <option value="RTGS">RTGS (Real Time Gross Settlement)</option>
        </select>
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
          <input type="text" class="search-box" id="inpLedgerSearch" placeholder="Filter by Account or JV..." oninput="filterLedger()">
          <button class="theme-toggle" onclick="exportLedgerCSV()">Export CSV</button>
          <button class="theme-toggle" onclick="refreshData()">⟳ Refresh</button>
        </div>
      </div>
      <div class="ledger-box">
        <table class="ledger-tbl">
          <thead>
            <tr>
              <th>Posting ID</th>
              <th>Created At</th>
              <th>Journal Voucher (JV ID)</th>
              <th>Account Number</th>
              <th>Direction</th>
              <th>Amount (₹)</th>
            </tr>
          </thead>
          <tbody id="ledgerTbody"></tbody>
        </table>
      </div>
    </div>
  </div>

  <div class="view-panel" id="viewACCOUNTS" style="padding:20px; overflow-y:auto; flex-direction:column;">
    <div class="section-title">Core Banking Chart of Accounts (init.sql)</div>
    <div class="ledger-box">
      <table class="ledger-tbl">
        <thead>
          <tr>
            <th>Account Identifier</th>
            <th>Account Name / Purpose</th>
            <th>Account Classification</th>
            <th>Base Currency</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td><code>00040310001928</code></td>
            <td><b>Corporate Operating Float (Demo Client)</b></td>
            <td><span style="color:var(--yellow); font-weight:700;">LIABILITY</span></td>
            <td>INR</td>
          </tr>
          <tr>
            <td><code>AC_CMS_SUSPENSE_CLEARING_9999</code></td>
            <td><b>CMS Intraday Clearing Suspense</b></td>
            <td><span style="color:var(--accent); font-weight:700;">SUSPENSE</span></td>
            <td>INR</td>
          </tr>
          <tr>
            <td><code>AC_RBI_NOSTRO_0001</code></td>
            <td><b>Central Bank Settlement Nostro</b></td>
            <td><span style="color:var(--green); font-weight:700;">ASSET</span></td>
            <td>INR</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>

  <div class="view-panel" id="viewTELEMETRY" style="padding:24px; flex-direction:column; gap:16px;">
    <div class="section-title">Core Infrastructure Telemetry</div>
    <div style="display:grid; grid-template-columns: repeat(3, 1fr); gap:16px;">
      <div class="card">
        <div class="card-label">Relational Core</div>
        <div class="card-val" style="color:var(--green);">PostgreSQL 16</div>
        <div style="font-size:11px; color:var(--muted); margin-top:6px;">Zero-Sum Trigger: ACTIVE (Enforced)</div>
      </div>
      <div class="card">
        <div class="card-label">Distributed Lien Engine</div>
        <div class="card-val" style="color:var(--green);">Redis 7.2 (Stack)</div>
        <div style="font-size:11px; color:var(--muted); margin-top:6px;">Lock-Free Lua Scripts • 24h Idempotency TTL</div>
      </div>
      <div class="card">
        <div class="card-label">Financial Messaging</div>
        <div class="card-val" style="color:var(--accent);">ISO 20022 Engine</div>
        <div style="font-size:11px; color:var(--muted); margin-top:6px;">pacs.008 Credit Transfer Serialization</div>
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
      document.getElementById("valSuspense").innerText = "₹" + Number(data.suspense_balance || 0).toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("valPostingsCount").innerText = data.postings ? data.postings.length : 0;
      cachedPostings = data.postings || [];
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
          "<td>#" + p.posting_id + "</td>" +
          "<td style='color:var(--muted);'>" + new Date(p.created_at).toLocaleTimeString() + "</td>" +
          "<td><b>" + p.jv_id + "</b></td>" +
          "<td><code>" + p.account_id + "</code></td>" +
          "<td><span class='" + (p.direction === "DR" ? "debit-tag" : "credit-tag") + "'>" + p.direction + "</span></td>" +
          "<td><b>₹" + Number(p.amount).toLocaleString('en-IN', {minimumFractionDigits: 2}) + "</b></td>" +
        "</tr>";
      });
    } else {
      tbody.innerHTML = "<tr><td colspan='6' style='text-align:center; padding:20px; color:var(--muted);'>No ledger postings recorded yet.</td></tr>";
    }
  }

  function filterLedger() {
    var q = document.getElementById("inpLedgerSearch").value.toLowerCase();
    var filtered = cachedPostings.filter(function(p) {
      return p.account_id.toLowerCase().includes(q) || p.jv_id.toLowerCase().includes(q);
    });
    renderLedgerTable(filtered);
  }

  function exportLedgerCSV() {
    if(!cachedPostings.length) return alert("No ledger postings available to export");
    var csv = "PostingID,CreatedAt,JVID,AccountID,Direction,Amount\n";
    cachedPostings.forEach(function(p) {
      csv += p.posting_id + ',"' + p.created_at + '","' + p.jv_id + '","' + p.account_id + '","' + p.direction + '",' + p.amount + '\n';
    });
    var blob = new Blob([csv], { type: "text/csv" });
    var url = window.URL.createObjectURL(blob);
    var a = document.createElement("a");
    a.href = url;
    a.download = "TBG_Core_Ledger_" + Date.now() + ".csv";
    a.click();
  }

  async function submitPayout() {
    var idemp = document.getElementById("inpIdemp").value;
    var payload = {
      corporate_account: document.getElementById("inpCorpAcc").value,
      amount: parseFloat(document.getElementById("inpAmount").value),
      currency: "INR",
      payment_rail: document.getElementById("inpRail").value,
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
      if(res.ok) {
        showToast("Settled! UTR: " + data.utr + " | JV: " + data.jv_id.slice(0, 8), false);
        generateNewIdemp();
        refreshData();
      } else {
        showToast(data.error || data.message || "Payout rejected by ledger", true);
      }
    } catch(err) {
      showToast("Engine Connection Error", true);
    }
  }
</script>
</body>
</html>`))

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

	// Seed float in Redis on startup for demo client
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

	// Serve the interactive workstation portal at root
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		portalTmpl.Execute(w, nil)
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
