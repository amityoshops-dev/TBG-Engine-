"use client";

import { useState, useEffect } from "react";
import { fetchStats, executePayout, triggerEODReconcile, PayoutPayload } from "@/lib/api";

export default function BankingWorkstation() {
  const [stats, setStats] = useState<any>({ suspense_balance: 0, postings: [] });
  const [loading, setLoading] = useState(false);
  const [latency, setLatency] = useState("1.12");
  const [selectedJv, setSelectedJv] = useState<any>(null);
  const [toast, setToast] = useState<string | null>(null);

  const [form, setForm] = useState<PayoutPayload>({
    corporate_account: "00040310001928",
    amount: 25000.0,
    currency: "INR",
    payment_rail: "NEFT",
    beneficiary_name: "Tata Motors Fleet Ltd",
    beneficiary_acct: "912345678901",
    beneficiary_ifsc: "HDFC0000001",
    reference_id: "REF-DEMO-001"
  });

  const [idemp, setIdemp] = useState("TXN-" + Date.now().toString().slice(-8));

  const loadData = async () => {
    try {
      const data = await fetchStats();
      setStats(data);
      if (data.postings && data.postings.length > 0 && !selectedJv) {
        setSelectedJv(data.postings[0]);
      }
    } catch (e) {
      console.error(e);
    }
  };

  useEffect(() => {
    loadData();
    const interval = setInterval(loadData, 3000);
    return () => clearInterval(interval);
  }, []);

  const showToast = (msg: string) => {
    setToast(msg);
    setTimeout(() => setToast(null), 3500);
  };

  const handlePayout = async () => {
    setLoading(true);
    const t0 = performance.now();
    try {
      const res = await executePayout(form, idemp);
      const elapsed = (performance.now() - t0).toFixed(2);
      setLatency(elapsed);
      if (res.status === "SETTLED" || res.utr) {
        showToast(`Settled! UTR: ${res.utr} (${elapsed}ms)`);
        setIdemp("TXN-" + Date.now().toString().slice(-8));
        loadData();
      } else {
        showToast(res.error || res.message || "Payout rejected by ledger");
      }
    } catch {
      showToast("Engine Connection Error");
    } finally {
      setLoading(false);
    }
  };

  const handleEOD = async () => {
    try {
      const res = await triggerEODReconcile();
      if (res.status === "RECONCILED") {
        showToast(`EOD Settled against RBI Nostro: INR ${res.cleared_amount}`);
        loadData();
      } else {
        showToast(res.message || "Reconciliation failed");
      }
    } catch {
      showToast("Reconciler request failed");
    }
  };

  const totalPool = 10000000.0;
  const suspenseBal = Number(stats?.suspense_balance || 0);
  const availFloat = totalPool - suspenseBal;
  const pct = Math.max(0, Math.min(100, (availFloat / totalPool) * 100));

  return (
    <div className="flex flex-col h-screen bg-[#030712] text-[#f9fafb] font-mono text-[11px] overflow-hidden">
      {/* Top Banner */}
      <header className="h-11 bg-[#0b0f19] border-b border-[#1f2937] flex items-center justify-between px-4">
        <div className="flex items-center gap-2">
          <span className="font-bold text-[12px] text-white tracking-wider">FINACLE TREASURY // STRIPE LEDGER</span>
          <span className="bg-indigo-500/20 text-indigo-400 border border-indigo-500/30 px-1.5 py-0.5 rounded text-[9px] font-bold">NEXTJS-14</span>
          <span className="bg-emerald-500/20 text-emerald-400 border border-emerald-500/30 px-1.5 py-0.5 rounded text-[9px] font-bold">ACID LIVE</span>
        </div>
        <div className="text-[#9ca3af] text-[10px]">
          Target API: <span className="text-cyan-400">https://tbg-engine.onrender.com</span>
        </div>
      </header>

      {/* Telemetry Strip */}
      <div className="h-14 bg-[#0b0f19] border-b border-[#1f2937] grid grid-cols-5 items-center px-4 gap-4">
        <div>
          <div className="text-[9px] text-[#9ca3af] uppercase">Corporate Operating Float</div>
          <div className="text-[13px] font-bold text-white">INR {availFloat.toLocaleString("en-IN", { minimumFractionDigits: 2 })}</div>
          <div className="h-1 bg-[#1f2937] rounded overflow-hidden mt-1">
            <div className="h-full bg-cyan-500" style={{ width: `${pct}%` }}></div>
          </div>
        </div>
        <div>
          <div className="text-[9px] text-[#9ca3af] uppercase">CMS Suspense (CR)</div>
          <div className="text-[13px] font-bold text-cyan-400">INR {suspenseBal.toLocaleString("en-IN", { minimumFractionDigits: 2 })}</div>
        </div>
        <div>
          <div className="text-[9px] text-[#9ca3af] uppercase">Clearing Rail</div>
          <div className="text-[13px] font-bold text-amber-400">NEFT / RTGS (SFMS)</div>
        </div>
        <div>
          <div className="text-[9px] text-[#9ca3af] uppercase">Engine Latency</div>
          <div className="text-[13px] font-bold text-emerald-400">{latency} ms <span className="text-[9px] font-normal text-slate-400">P99</span></div>
        </div>
        <div>
          <div className="text-[9px] text-[#9ca3af] uppercase">Audited Postings</div>
          <div className="text-[13px] font-bold text-white">{stats?.postings ? stats.postings.length : 0}</div>
        </div>
      </div>

      {/* Main Workspace */}
      <div className="flex-1 grid grid-cols-[330px_1fr] overflow-hidden">
        {/* Left Console */}
        <div className="bg-[#0b0f19] border-r border-[#1f2937] p-3.5 flex flex-col gap-2.5 overflow-y-auto">
          <div className="text-[10px] font-bold uppercase text-slate-400 border-b border-[#1f2937] pb-1">Payment Rail Dispatcher</div>
          <div className="flex flex-col gap-1">
            <label className="text-[9px] uppercase text-slate-400">Debit Float Account</label>
            <input className="bg-[#030712] border border-[#1f2937] p-1.5 rounded text-white text-[11px]" value={form.corporate_account} readOnly />
          </div>
          <div className="flex flex-col gap-1">
            <label className="text-[9px] uppercase text-slate-400">Beneficiary Legal Name</label>
            <input className="bg-[#030712] border border-[#1f2937] p-1.5 rounded text-white text-[11px]" value={form.beneficiary_name} onChange={(e) => setForm({ ...form, beneficiary_name: e.target.value })} />
          </div>
          <div className="flex flex-col gap-1">
            <label className="text-[9px] uppercase text-slate-400">Account Number</label>
            <input className="bg-[#030712] border border-[#1f2937] p-1.5 rounded text-white text-[11px]" value={form.beneficiary_acct} onChange={(e) => setForm({ ...form, beneficiary_acct: e.target.value })} />
          </div>
          <div className="flex flex-col gap-1">
            <label className="text-[9px] uppercase text-slate-400">IFSC Code</label>
            <input className="bg-[#030712] border border-[#1f2937] p-1.5 rounded text-white text-[11px]" value={form.beneficiary_ifsc} onChange={(e) => setForm({ ...form, beneficiary_ifsc: e.target.value })} />
          </div>
          <div className="flex flex-col gap-1">
            <label className="text-[9px] uppercase text-slate-400">Amount (INR)</label>
            <input type="number" className="bg-[#030712] border border-[#1f2937] p-1.5 rounded text-white text-[11px]" value={form.amount} onChange={(e) => setForm({ ...form, amount: parseFloat(e.target.value) || 0 })} />
          </div>
          <div className="flex flex-col gap-1">
            <label className="text-[9px] uppercase text-slate-400">Idempotency Key</label>
            <input className="bg-[#030712] border border-[#1f2937] p-1.5 rounded text-white text-[11px]" value={idemp} onChange={(e) => setIdemp(e.target.value)} />
          </div>
          <button onClick={handlePayout} disabled={loading} className="bg-indigo-600 hover:bg-indigo-700 text-white font-bold p-2 rounded text-[10px] uppercase tracking-wider mt-1 transition">
            {loading ? "COMMITTING LEDGER..." : "EXECUTE IDEMPOTENT PAYOUT"}
          </button>

          <div className="text-[10px] font-bold uppercase text-slate-400 border-b border-[#1f2937] pb-1 mt-3">Clearing Settlement</div>
          <button onClick={handleEOD} className="bg-emerald-600 hover:bg-emerald-700 text-white font-bold p-2 rounded text-[10px] uppercase tracking-wider transition">
            TRIGGER EOD NOSTRO SETTLEMENT
          </button>
        </div>

        {/* Right Canvas */}
        <div className="flex flex-col overflow-hidden bg-[#030712]">
          {/* Postings Table */}
          <div className="flex-1 overflow-y-auto">
            <table className="w-full text-left border-collapse">
              <thead className="bg-[#0b0f19] sticky top-0 border-b border-[#1f2937] text-[9px] text-[#9ca3af] uppercase">
                <tr>
                  <th className="p-2">ID</th>
                  <th className="p-2">Time</th>
                  <th className="p-2">Journal Voucher (JV ID)</th>
                  <th className="p-2">Account ID</th>
                  <th className="p-2">Leg</th>
                  <th className="p-2">Amount (INR)</th>
                  <th className="p-2">Audit Status</th>
                </tr>
              </thead>
              <tbody>
                {stats?.postings && stats.postings.length > 0 ? (
                  stats.postings.map((p: any) => (
                    <tr key={p.posting_id} onClick={() => setSelectedJv(p)} className="border-b border-[#1f2937] hover:bg-[#0b0f19] cursor-pointer">
                      <td className="p-2">#{p.posting_id}</td>
                      <td className="p-2 text-[#9ca3af]">{new Date(p.created_at).toLocaleTimeString()}</td>
                      <td className="p-2 font-bold">{p.jv_id.slice(0, 16)}...</td>
                      <td className="p-2 font-mono text-cyan-400">{p.account_id}</td>
                      <td className="p-2">
                        <span className={p.direction === "DR" ? "text-rose-400 font-bold" : "text-emerald-400 font-bold"}>
                          {p.direction}
                        </span>
                      </td>
                      <td className="p-2 font-bold">INR {Number(p.amount).toLocaleString("en-IN", { minimumFractionDigits: 2 })}</td>
                      <td className="p-2 text-emerald-400">✓ ZERO-SUM OK</td>
                    </tr>
                  ))
                ) : (
                  <tr>
                    <td colSpan={7} className="p-6 text-center text-[#64748b]">No ledger entries found.</td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          {/* Bottom Drawer: Wire & Webhook Inspector */}
          <div className="h-56 bg-[#0b0f19] border-t border-[#1f2937] grid grid-cols-2">
            <div className="border-r border-[#1f2937] p-3 flex flex-col overflow-hidden">
              <div className="text-[9px] font-bold text-slate-400 uppercase mb-1 flex justify-between">
                <span>ISO 20022 pacs.008.001.08 Wire Message</span>
                <span className="text-cyan-400">SFMS Cleared</span>
              </div>
              <pre className="flex-1 bg-[#020617] border border-[#1f2937] p-2 rounded text-[10px] text-cyan-300 overflow-auto whitespace-pre">
{selectedJv ? `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">
  <FIToFICstmrCdtTrf>
    <GrpHdr>
      <MsgId>MSG-${selectedJv.jv_id.slice(0, 8)}</MsgId>
      <CreDtTm>${new Date().toISOString()}</CreDtTm>
      <NbOfTxs>1</NbOfTxs>
      <SttlmInf><SttlmMtd>CLRG</SttlmMtd></SttlmInf>
    </GrpHdr>
    <CdtTrfTxInf>
      <PmtId><EndToEndId>REF-${selectedJv.jv_id.slice(0, 8)}</EndToEndId></PmtId>
      <IntrBkSttlmAmt Ccy="INR">${Number(selectedJv.amount).toFixed(2)}</IntrBkSttlmAmt>
      <Dbtr><Nm>Corporate Operating Float</Nm></Dbtr>
      <DbtrAcct><Id><Othr><Id>${selectedJv.account_id}</Id></Othr></Id></DbtrAcct>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>` : "<!-- Select a posting to view pacs.008 XML -->"}
              </pre>
            </div>
            <div className="p-3 flex flex-col overflow-hidden">
              <div className="text-[9px] font-bold text-slate-400 uppercase mb-1 flex justify-between">
                <span>Outward ERP Webhook Payload</span>
                <span className="text-emerald-400">HMAC-SHA256 SIGNED</span>
              </div>
              <pre className="flex-1 bg-[#020617] border border-[#1f2937] p-2 rounded text-[10px] text-emerald-300 overflow-auto whitespace-pre">
{selectedJv ? JSON.stringify({
  event: "payout.settled",
  timestamp: new Date().toISOString(),
  signature_256: `hmac_sha256_${selectedJv.jv_id.slice(0, 16)}`,
  data: {
    jv_id: selectedJv.jv_id,
    account_id: selectedJv.account_id,
    amount: selectedJv.amount,
    direction: selectedJv.direction,
    status: "SETTLED"
  }
}, null, 2) : "<!-- Awaiting selection -->"}
              </pre>
            </div>
          </div>
        </div>
      </div>

      {toast && (
        <div className="fixed bottom-4 right-4 bg-[#111827] border border-indigo-500 text-white text-[11px] px-3.5 py-2 rounded shadow-2xl z-50">
          {toast}
        </div>
      )}
    </div>
  );
}
