const BASE_URL = process.env.NEXT_PUBLIC_TBG_ENGINE_URL || "https://tbg-engine.onrender.com";

export interface PayoutPayload {
  corporate_account: string;
  amount: number;
  currency: string;
  payment_rail: string;
  beneficiary_name: string;
  beneficiary_acct: string;
  beneficiary_ifsc: string;
  reference_id: string;
}

export async function fetchStats() {
  const res = await fetch(`${BASE_URL}/api/v1/cms/stats`, { cache: 'no-store' });
  if (!res.ok) throw new Error("Failed to fetch engine stats");
  return res.json();
}

export async function executePayout(payload: PayoutPayload, idempotencyKey: string) {
  const res = await fetch(`${BASE_URL}/api/v1/cms/payout`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body: JSON.stringify(payload),
  });
  return res.json();
}

export async function triggerEODReconcile() {
  const res = await fetch(`${BASE_URL}/api/v1/cms/reconcile`, {
    method: "POST",
  });
  return res.json();
}
