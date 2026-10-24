// Adapted from @magpie-community/opencode-workbuddy-auth 0.1.25.
// Fixed provenance and intentional restrictions are recorded in SOURCE.md.
import { createHash, randomBytes, randomUUID } from "node:crypto"
import { validate, setSanitizer, sanitize } from "./contract.mjs"
const APP_VERSION = "2.0.0"
const UA_VERSION = "5.5.6"
const POLL_MS = 1000
const SIGN_IN_MS = 5 * 60 * 1000
const EARLY_MS = 60 * 1000
const LEAD_MS = 5 * 60 * 1000
const SITES = {
  workbuddy: {
    id: "workbuddy",
    name: "WorkBuddy",
    endpoint: "https://copilot.tencent.com",
    platform: "workbuddy",
    ai: false,
  },
  "workbuddy-ai": {
    id: "workbuddy-ai",
    name: "WorkBuddy AI",
    endpoint: "https://www.workbuddy.ai",
    platform: "workbuddy-ai",
    ai: true,
    // models whose images WorkBuddy AI drops though its config says they
    // take them (see textOnly)
    noImages: /^glm-/,
  },
}

function textOnly(site, model) {
  return !!site.noImages?.test(model)
}

const NPM = "@ai-sdk/openai-compatible"
const hex = () => randomBytes(16).toString("hex")
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const api = (site) => site.endpoint + "/v2"

class WBError extends Error {
  constructor(message, code, status) {
    super(message)
    this.code = code
    this.status = status
  }
}

// call makes a request of WorkBuddy's plugin API, which answers
// {code, msg, data}; a code but 0 is an error.
async function call(site, method, path, headers = {}, body) {
  const res = await fetch(site.endpoint + path, {
    method,
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": "WorkBuddy/" + UA_VERSION,
      ...headers,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30000),
  }).catch(() => { throw new Error(`${site.name} control request failed`) })
  const text = await res.text().catch(() => { throw new Error(`${site.name} control response could not be read`) })
  let env = {}
  try {
    env = JSON.parse(text)
  } catch {}
  const code = Number.parseInt(env.code, 10) || 0
  if (!res.ok) {
    throw new WBError(`${site.name} control request refused (HTTP ${res.status}, code ${code})`, code, res.status)
  }
  if (code !== 0) throw new WBError(`${site.name} control request refused (HTTP ${res.status}, code ${code})`, code, res.status)
  return env.data
}

function domainOf(site, a) {
  const domain = a.domain || new URL(api(site)).host
  if (typeof domain !== 'string' || domain.length > 253 || !/^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$/.test(domain)) throw new Error('invalid WorkBuddy account domain')
  return domain
}

// merge takes a token WorkBuddy gave (sign-in or refresh) into the
// stored sign-in, times given as at-ms or in-seconds.
function merge(a, got) {
  const now = Date.now()
  const out = { ...a, access: got.accessToken }
  if (got.refreshToken) out.refresh = got.refreshToken
  if (got.domain) out.domain = got.domain
  if (got.tokenType) out.tokenType = got.tokenType
  if (got.expiresAt > 0) out.expires = got.expiresAt
  else if (got.expiresIn > 0) out.expires = now + got.expiresIn * 1000
  if (got.refreshExpiresAt > 0) out.refreshExpiresAt = got.refreshExpiresAt
  else if (got.refreshExpiresIn > 0) out.refreshExpiresAt = now + got.refreshExpiresIn * 1000
  return out
}

function stale(a) {
  return !!a.expires && Date.now() >= a.expires - EARLY_MS
}

function sign(site, a, headers) {
  headers.set("Authorization", "Bearer " + a.access)
  headers.set("X-User-Id", a.uid ?? "")
  headers.set("X-Domain", domainOf(site, a))
  headers.set("X-Product", "SaaS")
  headers.set("X-IDE-Type", "WorkBuddy")
  headers.set("User-Agent", "WorkBuddy/" + UA_VERSION)
  headers.set("X-Requested-With", "XMLHttpRequest")
  headers.set("X-Agent-Intent", "craft")
  headers.set("X-Agent-Purpose", "conversation")
  headers.set("X-IDE-Name", "WorkBuddy")
  headers.set("X-IDE-Version", UA_VERSION)
  for (const [k, v] of Object.entries(enterpriseHeaders(a))) headers.set(k, v)
  if (!site.ai) return
  headers.set("X-Agent-Type", "main")
}

// enterpriseHeaders names the enterprise (team) an account signed in as, as
// WorkBuddy's own client does on every request it makes for that account
// (its auth interceptor: X-Enterprise-Id and X-Tenant-Id, both the
// account's enterpriseId). Without them WorkBuddy answers as the personal
// account: /v3/config lists the personal plan's models and not the
// enterprise's (#75). A personal account has none and sends none.
function enterpriseHeaders(a) {
  const id = typeof a?.enterpriseId === "string" ? a.enterpriseId.trim() : ""
  return id ? { "X-Enterprise-Id": id, "X-Tenant-Id": id } : {}
}

async function enterpriseOf(site, a, who) {
  const own = typeof who?.enterpriseId === "string" ? who.enterpriseId.trim() : ""
  if (own) return own
  const type = String(who?.type ?? "").trim().toLowerCase()
  if (!type || type === "personal") return ""
  let list
  try {
    list = await call(site, "GET", "/v2/plugin/accounts", {
      Authorization: "Bearer " + a.access,
      "X-Domain": domainOf(site, a),
    })
  } catch {
    throw new Error("WorkBuddy enterprise membership could not be verified")
  }
  const mine = (list?.accounts ?? []).filter((x) => x?.uid === who.uid && typeof x.enterpriseId === "string" && x.enterpriseId.trim())
  const same = mine.filter((x) => String(x.type ?? "").trim().toLowerCase() === type)
  const candidates = same.length ? same : mine
  const last = candidates.filter((x) => x.lastLogin)
  const pick = candidates.length === 1 ? candidates[0] : last.length === 1 ? last[0] : undefined
  if (!pick) throw new Error("WorkBuddy enterprise membership is ambiguous")
  return pick.enterpriseId.trim()
}

// idFor is the 32 hex digits WorkBuddy's own ids are made of, made of a
// name of the thing the id is for and what names it. The independent sign-in's
// private seed survives worker restarts and token rotations, not the token.
function idFor(sessionID, kind, key) {
  return createHash("sha256").update(`${sessionID}|${kind}|${key}`).digest("hex").slice(0, 32)
}

// signatureOf is a message content's signature: its text, and a digest of
// every part that isn't text (an image, a file), so a turn of images alone is
// a turn of its own too. A tool result's part — carried in a user message by
// clients of Anthropic's shape — is left out: it is not what the user said,
// and it changes with every step of the send.
function signatureOf(content) {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  let out = ""
  for (const part of content) {
    const type = part?.type ?? ""
    if (type === "" || type === "text") out += typeof part?.text === "string" ? part.text : ""
    else if (type !== "tool_result" && type !== "tool-result") {
      out += `[${type}:${createHash("sha256").update(JSON.stringify(part)).digest("hex").slice(0, 8)}]`
    }
  }
  return out
}

// answeredBefore is whether msgs[i] answers the message before it — a tool's
// result, or an assistant's calls — rather than carrying the user's own
// words. A client that sends a tool result's images in a user message (pi's
// "Attached image(s) from tool result:") puts that message right after the
// result, and a turn keyed on it would be a turn of its own on every step.
function answeredBefore(msgs, i) {
  const prev = msgs[i - 1]
  if (prev === null || typeof prev !== "object") return false
  if (prev.role === "tool") return true
  return prev.role === "assistant" && Array.isArray(prev.tool_calls) && prev.tool_calls.length > 0
}

// turnKey names the user send a chat answers: the body's last user message
// that is the user's own, by its place and its signature. Every step of one
// send repeats it; the user's next message changes it. "" for a chat whose
// last such message has nothing signable in it (an empty message) or which
// has none at all: the caller then makes a fresh id for that request, as
// before these were derived at all.
function turnKey(chat) {
  const msgs = Array.isArray(chat?.messages) ? chat.messages : []
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i]?.role !== "user" || answeredBefore(msgs, i)) continue
    const sig = signatureOf(msgs[i].content)
    return sig ? `u${i}:${sig}` : ""
  }
  return ""
}

// conversationKey names a conversation by the chat's first user message,
// which every later turn repeats. "" when no message names
// one — a body that isn't a chat, or one with no user message in it — and no
// conversation id is sent then: a made-up one would be a conversation of the
// request's own, which is what fragments the usage detail in the first place.
function conversationKey(chat) {
  const msgs = Array.isArray(chat?.messages) ? chat.messages : []
  for (const m of msgs) {
    if (m?.role !== "user") continue
    const sig = signatureOf(m.content)
    if (sig) return `first:${sig}`
  }
  return ""
}

// chatOf is a chat's body as the object it is, null for one that isn't: a
// body that isn't text (bytes), or isn't JSON at all.
function chatOf(body) {
  if (typeof body !== "string") return null
  try {
    const b = JSON.parse(body)
    return b !== null && typeof b === "object" ? b : null
  } catch {
    return null
  }
}

// attend sets a chat's ids on the headers its request goes to WorkBuddy
// with: the message's (each request its own, as the client makes them), the
// turn's (one user send, whatever it takes), and the conversation's. A header
// already there — the caller's own, or another plugin's — is kept, and the
// message and request ids are the same one, as WorkBuddy's client has them.
function attend(headers, body, sessionID) {
  const chat = chatOf(body)
  const conv = conversationKey(chat)
  const msg = hex()
  headers.set("X-Conversation-Message-ID", msg)
  headers.set("X-Request-ID", msg)
  if (!headers.has("X-Conversation-Request-ID")) {
    const turn = turnKey(chat)
    // a turn is a conversation's own: two conversations whose user message
    // reads the same are still two sends, not one
    headers.set("X-Conversation-Request-ID", turn ? idFor(sessionID, "turn", `${conv}|${turn}`) : hex())
  }
  if (conv && !headers.has("X-Conversation-ID")) headers.set("X-Conversation-ID", idFor(sessionID, "conversation", conv))
}

async function poll(site, path, headers, retry, deadline) {
  for (;;) {
    if (Date.now() > deadline) throw new Error(`${site.name} sign-in timed out`)
    try {
      const data = await call(site, "GET", path, headers)
      if (data && Object.keys(data).length) return data
    } catch (e) {
      if (!(retry.includes(e.code) || e.status === 408 || e.status === 429)) throw e
    }
    await sleep(POLL_MS)
  }
}

async function browserSignIn(site) {
  const state = await call(site, "POST", `/v2/plugin/auth/state?platform=${encodeURIComponent(site.platform)}`, {
    "X-No-Authorization": "true",
    "X-No-User-Id": "true",
    "X-No-Enterprise-Id": "true",
    "X-No-Department-Info": "true",
  }, {})
  if (!state?.state || !state?.authUrl) throw new Error(`${site.name} gave no sign-in page`)
  let url
  try { url = new URL(state.authUrl) } catch { throw new Error('WorkBuddy returned an invalid sign-in URL') }
  authorizeURL(site, url)
  url.searchParams.set("version", APP_VERSION)
  url.searchParams.set("loginSessionId", hex())
  return {
    url: url.toString(),
    instructions: `Sign in to ${site.name} in the browser; this finishes by itself.`,
    method: "auto",
    async callback() {
      try {
        const deadline = Date.now() + SIGN_IN_MS
        const q = encodeURIComponent(state.state)
        const token = await poll(site, `/v2/plugin/auth/token?state=${q}`, {}, [11217], deadline)
        if (!token.accessToken) return { type: "failed", error: `${site.name} sent back no token` }
        let a = merge({}, token)
        const who = await poll(site, `/v2/plugin/login/account?state=${q}`, {
          Authorization: "Bearer " + a.access,
          "X-Domain": domainOf(site, a),
          "X-No-User-Id": "true",
          "X-No-Enterprise-Id": "true",
        }, [12151], deadline)
        a.uid = who.uid ?? ""
        const enterpriseId = await enterpriseOf(site, a, who)
        if (enterpriseId) a.enterpriseId = enterpriseId
        return success(site, a)
      } catch (e) {
        return { type: "failed", error: sanitize(e.message) }
      }
    },
  }
}

function variants(efforts) {
  return Object.fromEntries((efforts ?? []).map((e) => [e, { reasoningEffort: e }]))
}

function modelOf(site, provider, m) {
  return {
    id: m.id,
    providerID: provider.id,
    name: m.name,
    api: { id: m.id, url: api(site), npm: NPM },
    status: "active",
    headers: {},
    options: {},
    cost: { input: 0, output: 0, cache: { read: 0, write: 0 } },
    limit: { context: m.context ?? 0, output: m.output ?? 0 },
    capabilities: {
      temperature: true,
      reasoning: !!m.efforts?.length,
      attachment: !!m.images,
      toolcall: true,
      input: { text: true, image: !!m.images, audio: false, video: false, pdf: false },
      output: { text: true, image: false, audio: false, video: false, pdf: false },
      interleaved: false,
    },
    release_date: "",
    variants: variants(m.efforts),
    // magpie's own: served at no cost to the plan's credits
    free: !!m.free,
    // magpie's own: the credits a request costs, as a multiple (0: none)
    rate: m.rate ?? 0,
    // magpie's own: the price before a discount running now (0: none)
    rateWas: m.rateWas ?? 0,
  }
}

// liveModels asks WorkBuddy's product config for the plan's models: the
// "cli" agent's list, each with its details. The CLI User-Agent picks
// WorkBuddy's config (a bare WorkBuddy/<v> gets CodeBuddy IDE's, no cli).
// creditsOf reads a model's credits, the multiple WorkBuddy's picker shows
// by it: "x0.03", "0" or 0; "x0.00" is free. A rate it can't read (or
// none, or an infinite one) is undefined: the model is neither free nor
// rated. The same rule as magpie's built-in (wbCredits).
function creditsOf(c) {
  if (c == null || c === "") return undefined
  const s = String(c).trim().toLowerCase().replace(/\s+credits?$/, "").replace(/^x/, "")
  const n = Number(s)
  return s !== "" && Number.isFinite(n) && n >= 0 ? n : undefined
}

// freeCredits says whether a model's credits are none: "x0.00", "0" or 0.
// A rate it can't read is not free.
function freeCredits(c) {
  return creditsOf(c) === 0
}

// discountOf reads a promotion's price: its discountedCredits ("0.50x",
// "0.00x"), else the factor times the model's own credits.
function discountOf(p, base) {
  const said = creditsOf(String(p.discount?.discountedCredits ?? "").trim().replace(/x$/i, ""))
  if (said !== undefined) return said
  const f = Number(p.discount?.factor)
  return Number.isFinite(f) && f >= 0 && base !== undefined ? base * f : undefined
}

// running says whether a promotion is on at now: enabled, and inside its
// validFrom..validUntil. Its daily hours are not looked at here.
function running(p, now) {
  if (!p || p.enabled === false) return false
  const from = Date.parse(p.schedule?.validFrom ?? "")
  const until = Date.parse(p.schedule?.validUntil ?? "")
  return !(from > now) && !(until <= now)
}

// priceOf is a model's price as magpie keeps it, from its credits and the
// config's modelPromotions (what WorkBuddy's picker shows as 限时免费,
// 夜间免费, 夜间折扣). magpie keeps the list it is given until it asks
// again, so a price that holds only some hours of the day (Hy4 preview,
// free 23:00–8:00 only) is given as its full price, never as free: a list
// asked for at night would otherwise say "free" all day (#121). A discount
// for whole days (Step-5-Preview's 独家限免) is the price now, with the
// price before it as rateWas.
function priceOf(d, promotions, now = Date.now()) {
  const base = creditsOf(d.credits)
  const mine = (Array.isArray(promotions) ? promotions : []).filter(
    (p) => p?.kind === "discount" && Array.isArray(p.modelIds) && p.modelIds.includes(d.id) && running(p, now),
  )
  if (mine.some((p) => Array.isArray(p.schedule?.daily) && p.schedule.daily.length)) {
    // by the hour: the full price, which the credits are unless a night
    // discount already took them to its own price
    const night = mine.filter((p) => p.discount && Array.isArray(p.schedule?.daily) && p.schedule.daily.length)
    if (base === undefined || (base === 0 && night.some((p) => discountOf(p, base) === 0))) return { free: false }
    return { free: false, rate: base }
  }
  const off = mine.filter((p) => p.discount).map((p) => discountOf(p, base)).filter((r) => r !== undefined)
  if (off.length) {
    const r = Math.min(...off)
    return { free: r === 0, rate: r, ...(base !== undefined && base > r ? { rateWas: base } : {}) }
  }
  if (base === undefined) return {}
  return { free: base === 0, rate: base }
}

async function liveModels(site, a) {
  const headers = new Headers()
  sign(site, a, headers)
  headers.set("User-Agent", `CLI/${APP_VERSION} WorkBuddy/${UA_VERSION}`)
  headers.set("X-Requested-With", "XMLHttpRequest")
  headers.set("Accept", "application/json")
  const res = await fetch(site.endpoint + "/v3/config", { headers, signal: AbortSignal.timeout(15000) })
  const env = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(`${site.name}'s config: HTTP ${res.status}`)
  if (Number(env.code)) throw new Error(`${site.name} config failed (HTTP ${res.status}, code ${Number.parseInt(env.code, 10) || 0})`)
  const cfg = env.data ?? {}
  const out = []
  for (const agent of cfg.agents ?? []) {
    if (agent.name !== "cli") continue
    for (const id of agent.models ?? []) {
      const d = (cfg.models ?? []).find((x) => x.id === id)
      const m = { id, name: d?.name || id }
      if (d) {
        m.context = d.maxInputTokens
        m.output = d.maxOutputTokens
        m.images = d.supportsImages === true && !textOnly(site, id)
        Object.assign(m, priceOf(d, cfg.modelPromotions))
        const es = d.reasoning?.supportedEfforts ?? []
        if (es.length) {
          const canOff = d.reasoning?.canDisableThinking
          m.efforts = [...(!d.onlyReasoning && (canOff == null || canOff) ? ["none"] : []), ...es]
        }
      }
      out.push(m)
    }
  }
  // tell apart models the config names alike: deepseek-v4.1-flash and
  // deepseek-v4.1-flash-sg are both "Deepseek-V4.1-Flash"
  const first = new Map()
  for (const m of out) {
    const id = first.get(m.name)
    if (id === undefined) {
      first.set(m.name, m.id)
      continue
    }
    const rest = m.id.startsWith(id + "-") ? m.id.slice(id.length + 1) : ""
    m.name = `${m.name} (${rest ? rest.toUpperCase() : m.id})`
  }
  return out
}


function authorizeURL(site, url) {
  const hosts = site.ai ? ['www.workbuddy.ai', 'www.codebuddy.ai'] : ['copilot.tencent.com', 'www.codebuddy.cn', 'www.workbuddy.cn']
  if (url.protocol !== 'https:' || url.username || url.password || url.port || !hosts.includes(url.hostname)) throw new Error('WorkBuddy returned an unverified sign-in URL')
}

function assertAuth(site, a) {
  if (a?.type !== 'oauth' || a.source != null || typeof a.uid !== 'string' || !a.uid.trim() || typeof a.access !== 'string' || !a.access || typeof a.refresh !== 'string' || !a.refresh || !Number.isFinite(a.expires) || a.expires <= 0) throw new Error('WorkBuddy requires independent browser authorization')
  if (typeof a.sessionID !== 'string' || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(a.sessionID)) throw new Error('invalid WorkBuddy authorization session identity')
  if (a.enterpriseId != null && (typeof a.enterpriseId !== 'string' || !a.enterpriseId.trim())) throw new Error('invalid WorkBuddy enterprise identity')
  domainOf(site, a)
}

// The host owns the account gate and durable version. No token cache can let
// later operations reuse a rotation whose persistence was not acknowledged.
async function renew(site, a) {
  assertAuth(site, a)
  if (a.refreshExpiresAt && Date.now() >= a.refreshExpiresAt) throw new Error('WorkBuddy authorization expired; sign in again')
  const got = await call(site, 'POST', '/v2/plugin/auth/token/refresh', {
    'X-Refresh-Token': a.refresh,
    'X-Auth-Refresh-Source': 'plugin',
    'X-Domain': domainOf(site, a),
    ...enterpriseHeaders(a),
  }, {})
  if (typeof got?.accessToken !== 'string' || !got.accessToken) throw new Error('WorkBuddy gave no refreshed token')
  const next = merge(a, got)
  assertAuth(site, next)
  return next
}

async function fresh(site, client, a) {
  assertAuth(site, a)
  if (!stale(a)) return a
  const next = await renew(site, a)
  await client.auth.set({ path: { id: site.id }, body: next })
  return next
}

function success(site, a) {
  a = { ...a, sessionID: randomUUID() }
  assertAuth(site, { ...a, type: 'oauth' })
  return { ...a, type: 'success', accountId: site.name }
}

function responseHeaders(res, type) {
  const headers = new Headers({ 'Content-Type': type, 'X-Magpie-Sign-In': 'kept' })
  const retry = res.headers.get('retry-after')
  // The Go gateway validates both delay-seconds and HTTP-date with ParseTime.
  // Preserve its input instead of discarding legal date-form cooldowns here.
  if (retry && retry.length <= 128) headers.set('Retry-After', retry)
  return headers
}

function errorBody(message) { return JSON.stringify({ error: { message: sanitize(message), type: 'upstream_error' } }) }
async function explained(res) {
  return new Response(errorBody((await res.text()).slice(0, 4096)), {
    status: res.status, headers: responseHeaders(res, 'application/json'),
  })
}

// Normal SSE data is forwarded unchanged. Only errors are rewritten through
// the host redactor. EOF and [DONE] cannot manufacture a model finish_reason.
function guardedStream(res) {
  const reader = res.body?.getReader()
  const decoder = new TextDecoder(), encoder = new TextEncoder()
  let buffer = '', ended = false, failed = false, doneMarker = false
  const choices = new Map()
  const terminal = () => choices.size > 0 && [...choices.values()].every(Boolean)
  return new Response(new ReadableStream({
    async pull(controller) {
      const emit = text => controller.enqueue(encoder.encode(text))
      const fail = message => { failed = true; emit('data: ' + errorBody(message) + '\n\n') }
      const frame = raw => {
        const lines = raw.split(/\r?\n/)
        const data = lines.filter(l => l.startsWith('data:')).map(l => l.slice(5).replace(/^ /, '')).join('\n')
        if (!data) return // comments and metadata cannot echo diagnostic secrets
        if (doneMarker) throw new Error('WorkBuddy sent data after its end marker')
        if (data === '[DONE]') {
          if (!terminal()) throw new Error('WorkBuddy stream ended without a model terminal')
          doneMarker = true
          emit(raw + '\n\n')
          return
        }
        let chunk
        try { chunk = JSON.parse(data) } catch { throw new Error('invalid WorkBuddy stream event') }
        if (lines.some(l => /^event:\s*error\s*$/.test(l)) || chunk.error || chunk.code != null && Number(chunk.code) !== 0) {
          throw new Error(typeof chunk.error === 'string' ? chunk.error : chunk.error?.message ?? chunk.msg ?? 'WorkBuddy stream error')
        }
        for (const c of chunk.choices ?? []) {
          const index = c.index ?? 0
          if (!Number.isInteger(index) || index < 0) throw new Error('invalid WorkBuddy choice index')
          if (choices.get(index)) throw new Error('WorkBuddy sent a choice after its terminal')
          choices.set(index, typeof c.finish_reason === 'string' && c.finish_reason.length > 0)
        }
        emit(raw + '\n\n')
      }
      try {
        while (!ended && !failed) {
          let at = buffer.search(/\r?\n\r?\n/)
          if (at >= 0) {
            const match = buffer.slice(at).match(/^\r?\n\r?\n/)[0]
            const raw = buffer.slice(0, at)
            buffer = buffer.slice(at + match.length)
            frame(raw)
            if (dataAvailable(raw)) return
            continue
          }
          if (!reader) { ended = true; break }
          const part = await reader.read()
          if (part.done) {
            buffer += decoder.decode()
            ended = true
            if (buffer.trim()) throw new Error('truncated WorkBuddy stream event')
            break
          }
          buffer += decoder.decode(part.value, { stream: true })
          if (buffer.length > 2 * 1024 * 1024) throw new Error('WorkBuddy stream event exceeds limit')
        }
        if (ended && !terminal()) fail('WorkBuddy stream ended without a model terminal')
      } catch (e) { fail(e.message) }
      if (ended || failed) { await reader?.cancel().catch(() => {}); controller.close() }
    },
    async cancel(reason) { await reader?.cancel(reason) },
  }), { status: res.status, headers: responseHeaders(res, 'text/event-stream') })
}
function dataAvailable(raw) { return raw.split(/\r?\n/).some(l => l.startsWith('data:')) }

function makePlugin(site) {
  return async ({ client }) => ({
    config: async config => {
      config.provider ??= {}
      config.provider[site.id] = { name: site.name, npm: NPM, api: api(site), models: {} }
    },
    provider: {
      id: site.id,
      async models(provider, { auth }) {
        const a = await fresh(site, client, auth)
        const models = await liveModels(site, a)
        if (!models.length) throw new Error('WorkBuddy returned no live models')
        return Object.fromEntries(models.map(m => [m.id, modelOf(site, provider, m)]))
      },
    },
    auth: {
      provider: site.id,
      refreshLead: LEAD_MS,
      refresh: auth => renew(site, auth),
      async loader(getAuth) {
        return {
          baseURL: api(site), apiKey: '',
          async fetch(input, init) {
            const req = input instanceof Request ? input : null
            const url = new URL(req ? req.url : input)
            if (url.href !== api(site) + '/chat/completions' || (init?.method ?? req?.method ?? 'GET') !== 'POST') throw new Error('unverified WorkBuddy inference endpoint')
            let body = init?.body
            if (body === undefined && req) body = await req.clone().text()
            const text = body instanceof ArrayBuffer || ArrayBuffer.isView(body) ? new TextDecoder().decode(body) : body
            let chat
            try { chat = JSON.parse(text) } catch { throw new Error('invalid WorkBuddy chat JSON') }
            validate(chat) // Before a token refresh or any other network.
            if (textOnly(site, chat.model) && chat.messages.some(m => Array.isArray(m.content) && m.content.some(p => p.type === 'image_url'))) throw new Error('WorkBuddy AI cannot preserve image inputs on this model')
            const a = await fresh(site, client, await getAuth())
            const headers = new Headers({ 'Content-Type': 'application/json', Accept: 'text/event-stream' })
            sign(site, a, headers)
            attend(headers, text, a.sessionID)
            let res
            try { res = await fetch(url.href, { method: 'POST', body, headers, signal: init?.signal ?? req?.signal }) }
            catch (e) { throw new Error(sanitize(e.message)) }
            if (!res.ok) return explained(res)
            if (!res.headers.get('content-type')?.toLowerCase().includes('text/event-stream')) {
              await res.body?.cancel()
              return new Response(errorBody('WorkBuddy did not return an event stream'), { status: 502, headers: responseHeaders(res, 'application/json') })
            }
            return guardedStream(res)
          },
        }
      },
      methods: [{ type: 'oauth', label: site.name + ' account (browser)', authorize: () => browserSignIn(site) }],
    },
  })
}

export const WorkBuddyAuthPlugin = makePlugin(SITES.workbuddy)
export const WorkBuddyAIAuthPlugin = makePlugin(SITES['workbuddy-ai'])
export const _torouter = { validate, setSanitizer }
export const _internal = { SITES, browserSignIn, fresh, guardedStream, domainOf, assertAuth }
