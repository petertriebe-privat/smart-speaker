// WhatsApp relay for Jarvis: the Cloud API delivers incoming messages only by webhook, the device
// sits behind the home router. This worker takes Meta's webhook (verify-token handshake,
// X-Hub-Signature-256 over the raw body), keeps the events in a Durable Object, and the device
// fetches them like Telegram's getUpdates: GET /poll?after=<last id>, which also drops everything
// up to that id. Replies go from the device straight to the Graph API, not through here.
// POST /event puts an announcement from a server of the household (the task agent's MCP server:
// "Ultron ist fertig: ...") into the same queue; the device speaks it, nothing else.
// GET /google is the redirect target of the device's Google login links (see googleLogin).
//
// Secrets (wrangler secret put): APP_SECRET (Meta app), VERIFY_TOKEN (chosen freely, also entered
// in Meta's webhook settings), DEVICE_TOKEN (the device's bearer token, also in secrets.h),
// EVENT_TOKEN (bearer token of /event, also on that server).

import { DurableObject } from "cloudflare:workers";

const MAX_BATCH = 20;
const MAX_WAIT_S = 25;
const MAX_ANNOUNCEMENT = 300;  // spoken at home
const MAX_CALL_REPORT = 1500;  // told in a call back, where the owner can ask

// Meta needs a privacy policy URL before the app can go live.
const PRIVACY = `<!doctype html><html lang="de"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Datenschutz – Jarvis</title><body style="font-family:sans-serif;max-width:40em;margin:2em auto;padding:0 1em;line-height:1.5">
<h1>Datenschutzhinweis</h1>
<p>Jarvis ist der private digitale Assistent eines Haushalts. Er beantwortet WhatsApp-Nachrichten von Mitgliedern dieses
Haushalts; Nachrichten anderer Absender werden nicht bearbeitet, sondern nur dem Inhaber gemeldet.</p>
<p>Eingehende Nachrichten werden über diesen Dienst (Cloudflare) an das Gerät des Haushalts weitergereicht und hier
gelöscht, sobald das Gerät sie abgeholt hat, spätestens nach sieben Tagen. Zur Beantwortung werden Inhalte an OpenAI
übermittelt (ohne Verwendung zum Training). Es findet keine Weitergabe zu Werbezwecken statt.</p>
<p>Auskunft und Löschung: über den WhatsApp-Chat mit Jarvis oder beim Inhaber des Haushalts.</p>
<h2>Privacy notice</h2>
<p>Jarvis is a household's private assistant. WhatsApp messages are relayed to the household's device, deleted here once
fetched (at the latest after seven days), and processed with OpenAI (not used for training). No data is sold or shared
for advertising.</p></body></html>`;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/webhook" && request.method === "GET") return verify(url, env);
    if (url.pathname === "/webhook" && request.method === "POST") return receive(request, env);
    if (url.pathname === "/poll" && request.method === "GET") return poll(request, url, env);
    if (url.pathname === "/event" && request.method === "POST") return announce(request, env);
    if (url.pathname === "/google" && request.method === "GET") return googleLogin(url, env);
    if (url.pathname === "/privacy") return new Response(PRIVACY, { headers: { "Content-Type": "text/html; charset=utf-8" } });
    return new Response("not found", { status: 404 });
  },
};

function inbox(env) {
  return env.INBOX.get(env.INBOX.idFromName("jarvis"));
}

// Meta's subscription check: echo hub.challenge if the token matches.
function verify(url, env) {
  const ok = url.searchParams.get("hub.mode") === "subscribe" &&
    equal(url.searchParams.get("hub.verify_token") || "", env.VERIFY_TOKEN || "");
  return ok ? new Response(url.searchParams.get("hub.challenge") || "") : new Response("forbidden", { status: 403 });
}

async function receive(request, env) {
  const body = await request.arrayBuffer();
  const signature = request.headers.get("X-Hub-Signature-256") || "";
  if (!(await signed(body, signature, env.APP_SECRET || ""))) return new Response("bad signature", { status: 401 });
  let payload;
  try {
    payload = JSON.parse(new TextDecoder().decode(body));
  } catch {
    return new Response("bad json", { status: 400 });
  }
  // Keep only what the device needs: changes with incoming messages. Delivery statuses of Jarvis'
  // own answers (sent, delivered, read) would only wake the device for nothing.
  const values = [];
  for (const entry of payload.entry || [])
    for (const change of entry.changes || [])
      if (change.field === "messages" && change.value?.messages?.length) values.push(change.value);
  if (values.length) await inbox(env).add(values);
  return new Response("ok");  // always 200 quickly, or Meta retries and finally disables the webhook
}

async function poll(request, url, env) {
  const auth = request.headers.get("Authorization") || "";
  if (!env.DEVICE_TOKEN || !equal(auth, "Bearer " + env.DEVICE_TOKEN)) return new Response("forbidden", { status: 403 });
  const after = Number(url.searchParams.get("after") || 0);
  const wait = Math.min(Math.max(Number(url.searchParams.get("wait") || 0), 0), MAX_WAIT_S);
  const events = await inbox(env).take(after, wait);
  return Response.json({ events });
}

// {"source": "Ultron", "text": "...", "deliver": "speak"|"call", "room": "..."}: queued as
// {"announcement": {...}}.
// Plain text only, spoken at home or told in a call to the admin: a leaked token can make the
// device talk (or ring the admin), not act.
async function announce(request, env) {
  const auth = request.headers.get("Authorization") || "";
  if (!env.EVENT_TOKEN || !equal(auth, "Bearer " + env.EVENT_TOKEN)) return new Response("forbidden", { status: 403 });
  let body;
  try {
    body = await request.json();
  } catch {
    return new Response("bad json", { status: 400 });
  }
  const text = String(body.text || "").trim();
  const source = String(body.source || "Server").trim().slice(0, 40);
  const deliver = body.deliver === "call" ? "call" : "speak";
  const room = String(body.room || "").trim().slice(0, 40); // the Jarvis speaker to say it on, "" = the device
  const max = deliver === "call" ? MAX_CALL_REPORT : MAX_ANNOUNCEMENT;
  if (!text || text.length > max) return new Response(`text: 1-${max} characters`, { status: 400 });
  await inbox(env).add([{ announcement: { source, text, deliver, room } }]);
  return new Response("ok");
}

// Google's redirect after the admin used a login link from the device (OAuth web client, PKCE):
// code and state are queued as {"google_login": {...}}. The device accepts only its own pending
// state, and the code is useless without the client secret and the PKCE verifier, which only the
// device has; so this needs no token.
async function googleLogin(url, env) {
  const code = url.searchParams.get("code") || "";
  const state = url.searchParams.get("state") || "";
  const ok = code && code.length <= 512 && /^[A-Za-z0-9]{32}$/.test(state);
  if (ok) await inbox(env).add([{ google_login: { code, state } }]);
  const text = ok ? "Danke! Jarvis übernimmt die Anmeldung und meldet sich gleich."
    : "Die Anmeldung wurde abgebrochen. Sie können den Link noch einmal öffnen.";
  return new Response(`<!doctype html><html lang="de"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Jarvis – Google</title><body style="font-family:sans-serif;max-width:30em;margin:3em auto;padding:0 1em;line-height:1.5">
<p>${text}</p></body></html>`, { status: ok ? 200 : 400, headers: { "Content-Type": "text/html; charset=utf-8" } });
}

async function signed(body, header, secret) {
  if (!secret || !header.startsWith("sha256=")) return false;
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" },
    false, ["sign"]);
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, body));
  const hex = [...mac].map((b) => b.toString(16).padStart(2, "0")).join("");
  return equal(header.slice(7), hex);
}

function equal(a, b) {
  const x = new TextEncoder().encode(a), y = new TextEncoder().encode(b);
  if (x.length !== y.length) return false;
  return crypto.subtle.timingSafeEqual(x, y);
}

// One queue for the household's number: SQLite storage (free plan), ids grow monotonically.
export class Inbox extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.sql.exec("CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER, body TEXT)");
    this.waiting = [];
  }

  async add(values) {
    const now = Date.now();
    for (const value of values) this.sql.exec("INSERT INTO events (at, body) VALUES (?, ?)", now, JSON.stringify(value));
    // Nothing fetched for a week (device gone): drop it rather than grow forever.
    this.sql.exec("DELETE FROM events WHERE at < ?", now - 7 * 24 * 3600 * 1000);
    const waiting = this.waiting;
    this.waiting = [];
    for (const wake of waiting) wake();
  }

  // Everything after `after` (and forget what came before); waits up to `wait` s if there is none.
  async take(after, wait) {
    this.sql.exec("DELETE FROM events WHERE id <= ?", after);
    let events = this.read(after);
    if (!events.length && wait > 0) {
      await new Promise((resolve) => {
        const timer = setTimeout(resolve, wait * 1000);
        this.waiting.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
      events = this.read(after);
    }
    return events;
  }

  read(after) {
    return this.sql.exec("SELECT id, at, body FROM events WHERE id > ? ORDER BY id LIMIT ?", after, MAX_BATCH)
      .toArray()
      .map((row) => ({ id: row.id, at: row.at, value: JSON.parse(row.body) }));
  }
}
