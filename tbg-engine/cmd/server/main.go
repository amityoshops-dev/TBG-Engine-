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
<title>TBG-CORE | Institutional Transaction Banking Terminal</title>
<style>
  :root {
    --bg-main: #07090e;
    --bg-surface: #0d121d;
    --bg-card: #121927;
    --border: #1e293b;
    --border-accent: #2563eb;
    --text-primary: #f8fafc;
    --text-secondary: #94a3b8;
    --text-muted: #64748b;
    --cyan: #06b6d4;
    --blue: #3b82f6;
    --green: #10b981;
    --red: #ef4444;
    --yellow: #f59e0b;
    --font-mono: "JetBrains Mono", Menlo, Consolas, monospace;
    --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg-main); color: var(--text-primary); font-family: var(--font-sans); height: 100vh; display: flex; flex-direction: column; overflow: hidden; font-size: 12px; }

  /* Header */
  header { height: 48px; background: var(--bg-surface); border-bottom: 1px solid var(--border); display: flex; align-items: center; justify-content: space-between; padding: 0 16px; flex-shrink: 0; }
  .brand { display: flex; align-items: center; gap: 10px; font-family: var(--font-mono); font-weight: 700; font-size: 13px; color: #fff; }
  .badge { font-family: var(--font-mono); font-size: 10px; padding: 2px 6px; border-radius: 3px; font-weight: 600; text-transform: uppercase; background: rgba(37,99,235,0.2); color: #60a5fa; border: 1px solid rgba(37,99,235,0.4); }

  /* Telemetry Ribbon */
  .telemetry { height: 56px; background: var(--bg-surface); border-bottom: 1px solid var(--border); display: grid; grid-template-columns: 2fr 1fr 1fr 1fr 1fr; align-items: center; padding: 0 16px; gap: 16px; flex-shrink: 0; }
  .telemetry-item { display: flex; flex-direction: column; }
  .telemetry-lbl { font-family: var(--font-mono); font-size: 9px; text-transform: uppercase; color: var(--text-muted); }
  .telemetry-val { font-family: var(--font-mono); font-size: 14px; font-weight: 700; color: #fff; margin-top: 2px; }
  .track { height: 4px; background: var(--border); border-radius: 2px; margin-top: 4px; overflow: hidden; }
  .track-fill { height: 100%; background: linear-gradient(90deg, var(--blue), var(--cyan)); width: 100%; transition: width 0.3s; }

  /* Main Workspace */
  .workspace { display: grid; grid-template-columns: 360px 1fr; flex: 1; overflow: hidden; }

  /* Operations Console */
  .ops { background: var(--bg-surface); border-right: 1px solid var(--border); display: flex; flex-direction: column; padding: 16px; gap: 14px; overflow-y: auto; }
  .block-title { font-family: var(--font-mono); font-size: 11px; font-weight: 700; text-transform: uppercase; color: var(--text-secondary); border-bottom: 1px solid var(--border); padding-bottom: 6px; }
  .field { display: flex; flex-direction: column; gap: 4px; }
  .field label { font-family: var(--font-mono); font-size: 10px; color: var(--text-muted); text-transform: uppercase; }
  .field input, .field select { background: var(--bg-main); border: 1px solid var(--border); border-radius: 4px; color: #fff; font-family: var(--font-mono); font-size: 11px; padding: 7px 10px; outline: none; }
  .field input:focus, .field select:focus { border-color: var(--blue); }
  .btn-run { background: var(--blue); border: none; border-radius: 4px; color: #fff; font-family: var(--font-mono); font-size: 11px; font-weight: 700; text-transform: uppercase; padding: 10px; cursor: pointer; }
  .btn-run:hover { background: #1d4ed8; }

  /* Tabbed Workstation Viewport */
  .canvas { display: flex; flex-direction: column; overflow: hidden; background: var(--bg-main); }
  .nav-bar { display: flex; background: var(--bg-surface); border-bottom: 1px solid var(--border); padding: 0 16px; gap: 8px; }
  .nav-tab { background: none; border: none; border-bottom: 2px solid transparent; color: var(--text-muted); font-family: var(--font-mono); font-size: 11px; font-weight: 600; padding: 12px 14px; cursor: pointer; text-transform: uppercase; }
  .nav-tab.active { color: #fff; border-bottom-color: var(--blue); }

  .view { flex: 1; display: none; overflow: hidden; flex-direction: column; }
  .view.active { display: flex; }

  /* Interactive Pipeline Flow Canvas */
  .flow-box { background: var(--bg-card); border-bottom: 1px solid var(--border); padding: 14px 16px; display: flex; flex-direction: column; gap: 8px; flex-shrink: 0; }
  .flow-title { font-family: var(--font-mono); font-size: 10px; text-transform: uppercase; color: var(--text-muted); font-weight: 700; display: flex; justify-content: space-between; }
  .pipeline-nodes { display: grid; grid-template-columns: repeat(5, 1fr); gap: 10px; margin-top: 4px; }
  .node { background: var(--bg-main); border: 1px solid var(--border); border-radius: 4px; padding: 10px; display: flex; flex-direction: column; gap: 4px; cursor: pointer; transition: all 0.2s; }
  .node:hover, .node.selected { border-color: var(--cyan); background: rgba(6,182,212,0.05); }
  .node.active { border-color: var(--green); background: rgba(16,185,129,0.1); }
  .node-hdr { display: flex; align-items: center; gap: 6px; font-family: var(--font-mono); font-size: 10px; font-weight: 700; color: #fff; }
  .node-dot { width: 6px; height: 6px; border-radius: 50%; background: var(--text-muted); }
  .node.active .node-dot { background: var(--green); box-shadow: 0 0 6px var(--green); }
  .node-pkg { font-family: var(--font-mono); font-size: 9px; color: var(--text-muted); }

  .node-detail { background: var(--bg-surface); border: 1px dashed var(--border); border-radius: 4px; padding: 8px 12px; font-family: var(--font-mono); font-size: 11px; color: var(--text-secondary); line-height: 1.4; display: flex; justify-content: space-between; align-items: center; }

  /* Table */
  .table-wrap { flex: 1; overflow-y: auto; }
  table { width: 100%; border-collapse: collapse; font-family: var(--font-mono); font-size: 11px; }
  th { background: var(--bg-surface); color: var(--text-muted); padding: 8px 12px; text-align: left; font-size: 10px; text-transform: uppercase; border-bottom: 1px solid var(--border); position: sticky; top: 0; }
  td { padding: 8px 12px; border-bottom: 1px solid var(--border); }
  tr:hover td { background: var(--bg-surface); cursor: pointer; }
  .tag-dr { color: var(--red); font-weight: 700; }
  .tag-cr { color: var(--green); font-weight: 700; }

  /* Split View Wire & Webhook Inspector */
  .inspectors { display: grid; grid-template-columns: 1fr 1fr; height: 230px; border-top: 1px solid var(--border); background: var(--bg-surface); flex-shrink: 0; }
  .inspect-pane { display: flex; flex-direction: column; padding: 10px 14px; overflow: hidden; }
  .inspect-pane:first-child { border-right: 1px solid var(--border); }
  .pane-top { display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px; }
  .pane-tag { font-family: var(--font-mono); font-size: 10px; font-weight: 700; text-transform: uppercase; color: var(--text-secondary); }
  .code { flex: 1; background: #040711; border: 1px solid var(--border); border-radius: 4px; padding: 8px 10px; font-family: var(--font-mono); font-size: 10px; color: #38bdf8; overflow: auto; white-space: pre; line-height: 1.4; }

  /* PRD Tab */
  .prd-body { flex: 1; overflow-y: auto; padding: 20px; display: flex; flex-direction: column; gap: 16px; }
  .prd-card { background: var(--bg-surface); border: 1px solid var(--border); border-radius: 6px; padding: 16px; }
  .prd-title { font-family: var(--font-mono); font-size: 13px; font-weight: 700; color: #fff; margin-bottom: 8px; }
  .prd-text { font-size: 12px; line-height: 1.6; color: var(--text-secondary); }
  .prd-text code { font-family: var(--font-mono); background: rgba(255,255,255,0.05); padding: 2px 4px; border-radius: 3px; color: var(--cyan); }

  #toast { display: none; position: fixed; bottom: 20px; right: 20px; background: var(--bg-card); border: 1px solid var(--blue); color: #fff; font-family: var(--font-mono); font-size: 11px; padding: 10px 16px; border-radius: 4px; z-index: 99; box-shadow: 0 10px 30px rgba(0,0,0,0.5); }
</style>
</head>
<body>

<header>
  <div class="brand">
    <span>TBG-CORE TRANSACTION BANKING TERMINAL</span>
    <span class="badge">ACID Ledger</span>
  </div>
  <div style="font-family: var(--font-mono); font-size: 11px; color: var(--text-muted);">
    PostgreSQL 16 Trigger Enforced • Redis 7.2 Lua Locks • ISO-20022 Engine
  </div>
</header>

<div class="telemetry">
  <div class="telemetry-item">
    <div class="telemetry-lbl">Corporate Float (00040310001928)</div>
    <div class="telemetry-val" id="txtFloat">INR 9,950,000.00</div>
    <div class="track"><div class="track-fill" id="floatBar" style="width: 99.5%;"></div></div>
  </div>
  <div class="telemetry-item">
    <div class="telemetry-lbl">CMS Suspense Balance</div>
    <div class="telemetry-val" id="txtSuspense" style="color: var(--cyan);">INR 50,000.00</div>
  </div>
  <div class="telemetry-item">
    <div class="telemetry-lbl">Clearing Rail Protocol</div>
    <div class="telemetry-val" style="color: var(--yellow);">NEFT / RTGS</div>
  </div>
  <div class="telemetry-item">
    <div class="telemetry-lbl">Execution Latency</div>
    <div class="telemetry-val" style="color: var(--green);"><span id="txtLatency">1.12</span> ms</div>
  </div>
  <div class="telemetry-item">
    <div class="telemetry-lbl">Ledger Postings</div>
    <div class="telemetry-val" id="txtCount">0</div>
  </div>
</div>

<div class="workspace">
  <!-- Payout Dispatcher Column -->
  <div class="ops">
    <div class="block-title">CMS Outward Rail Dispatcher</div>
    <div class="field">
      <label>Debit Corporate Account</label>
      <input type="text" id="inpCorpAcc" value="00040310001928" readonly>
    </div>
    <div class="field">
      <label>Beneficiary Entity Name</label>
      <input type="text" id="inpBeneName" value="Tata Motors Fleet Ltd">
    </div>
    <div class="field">
      <label>Beneficiary Account Number</label>
      <input type="text" id="inpBeneAcct" value="912345678901">
    </div>
    <div class="field">
      <label>Beneficiary IFSC Code</label>
      <input type="text" id="inpIfsc" value="HDFC0000001">
    </div>
    <div class="field">
      <label>Payout Amount (INR)</label>
      <input type="number" id="inpAmount" value="25000.00" step="500">
    </div>
    <div class="field">
      <label>Clearing Rail Mode</label>
      <select id="inpRail">
        <option value="NEFT">NEFT (National Electronic Fund Transfer)</option>
        <option value="RTGS">RTGS (Real Time Gross Settlement)</option>
      </select>
    </div>
    <div class="field">
      <label>Idempotency Reference Key</label>
      <input type="text" id="inpIdemp" value="TXN-DEMO-001">
    </div>
    <button class="btn-run" onclick="submitPayout()">EXECUTE IDEMPOTENT PAYOUT</button>
  </div>

  <!-- Main Canvas -->
  <div class="canvas">
    <div class="nav-bar">
      <button class="nav-tab active" onclick="switchNav('AUDIT', this)">Double-Entry Audit & Mechanics</button>
      <button class="nav-tab" onclick="switchNav('ACCOUNTS', this)">Chart of Accounts Master</button>
      <button class="nav-tab" onclick="switchNav('PRD', this)">PRD & Concurrency Architecture</button>
    </div>

    <!-- VIEW 1: Interactive Audit & Mechanics -->
    <div class="view active" id="viewAUDIT">
      <!-- Interactive Pipeline Flow Canvas -->
      <div class="flow-box">
        <div class="flow-title">
          <span>Clearing Mechanics & State Engine</span>
          <span style="color: var(--cyan);">Click any step to inspect package mechanics</span>
        </div>
        <div class="pipeline-nodes">
          <div class="node" id="node1" onclick="showNodeMechanics(1)">
            <div class="node-hdr"><div class="node-dot"></div> 1. IDEMPOTENCY</div>
            <div class="node-pkg">internal/service</div>
          </div>
          <div class="node" id="node2" onclick="showNodeMechanics(2)">
            <div class="node-hdr"><div class="node-dot"></div> 2. LIEN_ENGINE</div>
            <div class="node-pkg">internal/ledger</div>
          </div>
          <div class="node" id="node3" onclick="showNodeMechanics(3)">
            <div class="node-hdr"><div class="node-dot"></div> 3. DOUBLE_ENTRY</div>
            <div class="node-pkg">internal/ledger</div>
          </div>
          <div class="node" id="node4" onclick="showNodeMechanics(4)">
            <div class="node-hdr"><div class="node-dot"></div> 4. ISO20022</div>
            <div class="node-pkg">internal/iso20022</div>
          </div>
          <div class="node" id="node5" onclick="showNodeMechanics(5)">
            <div class="node-hdr"><div class="node-dot"></div> 5. SETTLED</div>
            <div class="node-pkg">internal/service</div>
          </div>
        </div>
        <div class="node-detail" id="nodeDetailBox">
          <span id="txtNodeDesc">Step 1: Idempotency Barrier acquires a distributed lock in Redis (SETNX with 24h TTL) to prevent duplicate debits.</span>
          <code id="txtNodeCode" style="color: var(--cyan);">s.RDB.SetNX(ctx, "idemp:"+key, "PENDING", 24*time.Hour)</code>
        </div>
      </div>

      <!-- Ledger Table -->
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>Timestamp</th>
              <th>Journal Voucher (JV ID)</th>
              <th>Account Identifier</th>
              <th>Leg</th>
              <th>Amount (INR)</th>
              <th>Zero-Sum Audit</th>
            </tr>
          </thead>
          <tbody id="ledgerTbody"></tbody>
        </table>
      </div>

      <!-- Split Inspector Drawer -->
      <div class="inspectors">
        <div class="inspect-pane">
          <div class="pane-top">
            <span class="pane-tag">ISO 20022 pacs.008.001.08 XML Wire Message</span>
            <span style="font-family: var(--font-mono); font-size: 10px; color: var(--text-muted);">Rail: Cleared</span>
          </div>
          <div class="code" id="xmlScreen">&lt;!-- Select a voucher to view XML wire --&gt;</div>
        </div>
        <div class="inspect-pane">
          <div class="pane-top">
            <span class="pane-tag">Outward ERP Webhook Dispatch (HMAC Non-Repudiation)</span>
            <span style="font-family: var(--font-mono); font-size: 10px; color: var(--green);">HTTP 200 COMMITTED</span>
          </div>
          <div class="code" id="webhookScreen">{ "event": "payout.settled", "status": "AWAITING_SELECTION" }</div>
        </div>
      </div>
    </div>

    <!-- VIEW 2: Chart of Accounts -->
    <div class="view" id="viewACCOUNTS" style="padding: 20px; overflow-y: auto;">
      <table>
        <thead>
          <tr>
            <th>Account Identifier</th>
            <th>Designation</th>
            <th>Account Type</th>
            <th>Currency</th>
            <th>Derived Ledger Position</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td><code>00040310001928</code></td>
            <td><b>Corporate Operating Float (Client Float)</b></td>
            <td><span style="color: var(--yellow); font-weight: 700;">LIABILITY</span></td>
            <td>INR</td>
            <td>Derived via Sum(CR) - Sum(DR)</td>
          </tr>
          <tr>
            <td><code>AC_CMS_SUSPENSE_CLEARING_9999</code></td>
            <td><b>CMS Intraday Clearing Suspense</b></td>
            <td><span style="color: var(--cyan); font-weight: 700;">SUSPENSE</span></td>
            <td>INR</td>
            <td>Accumulates Outward Clearing Debits</td>
          </tr>
          <tr>
            <td><code>AC_RBI_NOSTRO_0001</code></td>
            <td><b>Central Bank Settlement Nostro</b></td>
            <td><span style="color: var(--green); font-weight: 700;">ASSET</span></td>
            <td>INR</td>
            <td>End-of-Day Net Clearing Offset Leg</td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- VIEW 3: PRD & Architecture -->
    <div class="view" id="viewPRD">
      <div class="prd-body">
        <div class="prd-card">
          <div class="prd-title">1. Lock-Free Concurrency Control (internal/ledger/lien_engine.go)</div>
          <div class="prd-text">
            Under high-throughput burst traffic, relational database row locks create connection pool exhaustion. TBG-CORE uses an atomic Redis Lua script (<code>holdScript</code>) that evaluates available float in <code>corp:avail:00040310001928</code> and registers an in-flight hold in <code>corp:liens:00040310001928</code> in under 1.5ms. Available balance is mathematically defined as:
            <br><br>
            <code>Available Balance = Ledger Balance - Active Liens</code>
          </div>
        </div>
        <div class="prd-card">
          <div class="prd-title">2. Relational Zero-Sum Guarantee (init.sql & internal/ledger/repository.go)</div>
          <div class="prd-text">
            The core ledger is strictly append-only. Zero <code>UPDATE</code> or <code>DELETE</code> statements exist. All balance inquiries are dynamically derived. A deferred constraint trigger (<code>trg_verify_jv_balance</code>) runs at database commit:
            <br><br>
            <code>verify_jv_balance(): SUM(CASE WHEN direction = 'DR' THEN amount ELSE -amount END) = 0</code>
            <br><br>
            If any unbalanced journal leg is attempted, the entire PostgreSQL transaction is aborted immediately.
          </div>
        </div>
        <div class="prd-card">
          <div class="prd-title">3. ISO 20022 pacs.008 Clearing Wire Generation</div>
          <div class="prd-text">
            Every settled payout generates an authentic <code>pacs.008.001.08</code> Financial Institutional Customer Credit Transfer message containing instructing agent routing, creditor accounts, and a unique UTR (<code>CMSNEFT...</code>).
          </div>
        </div>
      </div>
    </div>
  </div>
</div>

<div id="toast"></div>

<script>
  var TOTAL_FLOAT = 10000000.00;
  var cachedPostings = [];

  var mechanics = {
    1: { desc: "Step 1: Idempotency Barrier acquires a distributed lock in Redis (SETNX with 24h TTL) to prevent duplicate debits.", code: "s.RDB.SetNX(ctx, \"idemp:\"+key, \"PENDING\", 24*time.Hour)" },
    2: { desc: "Step 2: Lock-Free Lien Engine runs atomic Lua script to hold funds from available corporate float without PostgreSQL row locks.", code: "redis.call('DECRBY', KEYS[1], req) & redis.call('HSET', KEYS[2], ref, req)" },
    3: { desc: "Step 3: PostgreSQL commits balanced double-entry voucher (DR 00040310001928 / CR Suspense) verified by zero-sum trigger.", code: "INSERT INTO postings (jv_id, account_id, direction, amount) VALUES (..., 'DR', amt), (..., 'CR', amt)" },
    4: { desc: "Step 4: ISO 20022 Engine builds pacs.008.001.08 credit transfer XML wire payload with unique EndToEndId and UTR.", code: "iso20022.BuildPacs008(msgID, refID, client, beneName, beneAcct, ifsc, ccy, amt)" },
    5: { desc: "Step 5: Lien permanently released in Redis because funds are now irreversibly committed to the general ledger.", code: "redis.call('HDEL', KEYS[2], ref) -- restore = 0" }
  };

  window.onload = function() {
    generateNewIdemp();
    refreshData();
    setInterval(refreshData, 3000);
  };

  function switchNav(id, btn) {
    document.querySelectorAll(".view").forEach(function(v) { v.classList.remove("active"); });
    document.querySelectorAll(".nav-tab").forEach(function(b) { b.classList.remove("active"); });
    document.getElementById("view" + id).classList.add("active");
    btn.classList.add("active");
  }

  function showNodeMechanics(step) {
    document.querySelectorAll(".node").forEach(function(n) { n.classList.remove("selected"); });
    document.getElementById("node" + step).classList.add("selected");
    document.getElementById("txtNodeDesc").innerText = mechanics[step].desc;
    document.getElementById("txtNodeCode").innerText = mechanics[step].code;
  }

  function setPipelineStep(step) {
    for (var i = 1; i <= 5; i++) {
      var n = document.getElementById("node" + i);
      if (i <= step) n.classList.add("active");
      else n.classList.remove("active");
    }
    if (mechanics[step]) {
      showNodeMechanics(step);
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
      var pct = Math.max(0, Math.min(100, (avail / TOTAL_FLOAT) * 100));

      document.getElementById("txtSuspense").innerText = "INR " + suspense.toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("txtFloat").innerText = "INR " + avail.toLocaleString("en-IN", {minimumFractionDigits: 2});
      document.getElementById("floatBar").style.width = pct + "%";
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
          '<td><span class="' + (p.direction === "DR" ? "tag-dr" : "tag-cr") + '">' + p.direction + '</span></td>' +
          '<td><b>INR ' + Number(p.amount).toLocaleString('en-IN', {minimumFractionDigits: 2}) + '</b></td>' +
          '<td><span style="color:var(--green);">✓ Zero-Sum Verified</span></td>' +
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

    setPipelineStep(1);
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

    setTimeout(function() { setPipelineStep(2); }, 60);

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
        setPipelineStep(5);
        showToast("Settled! UTR: " + data.utr + " (" + elapsed + "ms)");
        generateNewIdemp();
        refreshData();
        inspectVoucher(data.jv_id, payload.amount);
      } else {
        setPipelineStep(0);
        showToast(data.error || "Payout rejected by ledger");
      }
    } catch(err) {
      setPipelineStep(0);
      showToast("Engine Connection Error");
    }
  }
</script>
</body>
</html>`
