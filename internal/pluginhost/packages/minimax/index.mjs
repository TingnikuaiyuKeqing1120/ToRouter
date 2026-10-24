// OpenCode provider plugins for MiniMax Code (mcode) accounts: the credits
// a MiniMax Code account gets and its
// M Plan, on MiniMax Code's own models.
//   - minimax-code:        the China site (account.minimax.cn, agent.minimax.cn)
//   - minimax-code-global: the international site (account.minimax.io, agent.minimax.io)
// An account signs in here, with MiniMax Code's device sign-in (OAuth 2
// device flow with PKCE, client mcode-public): nothing is read from or
// written to MiniMax Code's own sign-in (~/.minimax). Its chats are
// Anthropic messages at <agent>/mavis/api/v1/llm/v1/messages, sent with
// MiniMax Code's headers. to-router exposes independent authorization only.

import { createHash, randomBytes, randomUUID } from "node:crypto"
import { validate, setSanitizer, sanitize, answer } from "./contract.mjs"

const CLIENT_ID = "mcode-public"
const SCOPE = "agent.default"
const AUDIENCE = "agent-backend"
const DEVICE_GRANT = "urn:ietf:params:oauth:grant-type:device_code"
const EARLY_MS = 2 * 60 * 1000 // a token this close to its end is refreshed before a request
const LEAD_MS = 10 * 60 * 1000 // and this close, magpie renews it ahead of time (auth.refresh)
const SIGN_IN_MS = 10 * 60 * 1000 // when the device code gives no end of its own
const SIGN_SECRET = "I*7Cf%WZ#S&%1RlZJ&C2" // MiniMax Code's request signing (x-signature)
const NPM = "@ai-sdk/anthropic"
// the key the Anthropic SDK is given in MiniMax Code: a placeholder, the
// account going in Authorization
const PLACEHOLDER_KEY = "sk-xxx"

// Fixed regional hosts from the reviewed source. Tests replace fetch only.
const SITES = {
  "minimax-code": {
    id: "minimax-code",
    name: "MiniMax Code",
    region: "cn",
    lang: "zh",
    account: "https://account.minimax.cn", // OAuth
    llm: "https://agent.minimax.cn", // chats and the model list
    agent: "https://agent.minimaxi.com", // stable identity
  },
  "minimax-code-global": {
    id: "minimax-code-global",
    name: "MiniMax Code (Global)",
    region: "en",
    lang: "en",
    account: "https://account.minimax.io",
    llm: "https://agent.minimax.io",
    agent: "https://agent.minimax.io",
  },
}

const api = (site) => site.llm + "/mavis/api/v1/llm/v1"
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const md5 = (s) => createHash("md5").update(s).digest("hex")
const str = (o, k) => (typeof o?.[k] === "string" && o[k].trim() ? o[k].trim() : undefined)
const pos = (o, k) => (typeof o?.[k] === "number" && Number.isFinite(o[k]) && o[k] > 0 ? o[k] : undefined)
const obj = (v) => (v && typeof v === "object" && !Array.isArray(v) ? v : undefined)
const num = (v) => {
  if (typeof v === "number" && Number.isFinite(v)) return v
  if (typeof v === "string" && v.trim() && Number.isFinite(Number(v))) return Number(v)
  return undefined
}

// ---- the models -------------------------------------------------------------

// Native Messages are forwarded without model-parameter or history lowering.
function variants(m) {
  const out = {}
  if (m.thinking === "switchable") out.none = { thinking: { type: "disabled" } }
  if (m.efforts?.length) for (const effort of m.efforts) out[effort] = { thinking: { type: "adaptive" }, effort }
  else if (m.thinking === "switchable") out.high = { thinking: { type: "adaptive" } }
  return out
}
const reasons = (m) => m.thinking !== "off"

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
      reasoning: reasons(m),
      attachment: !!m.images,
      toolcall: true,
      input: { text: true, image: !!m.images, audio: false, video: false, pdf: false },
      output: { text: true, image: false, audio: false, video: false, pdf: false },
      interleaved: false,
    },
    release_date: "",
    variants: variants(m),
  }
}

// parseModels reads MiniMax Code's model config ({providers:[{providerId:
// "minimax", config:{models:{<id>:{…}}, model_order?}}]}) as models.
function parseModels(body) {
  const p = (Array.isArray(body?.providers) ? body.providers : []).find((x) => x?.providerId === "minimax")
  const ms = obj(obj(p?.config)?.models)
  if (!ms) return []
  const order = Array.isArray(p.config.model_order) ? p.config.model_order.filter((id) => ms[id]) : []
  const ids = [...order, ...Object.keys(ms).filter((id) => !order.includes(id))]
  const out = []
  for (const id of ids) {
    const d = obj(ms[id])
    if (!d || !id.trim()) continue
    const input = Array.isArray(d.modalities?.input) ? d.modalities.input : []
    const mode = str(d.thinking_config, "mode")
    const efforts = (d.thinking?.effortOptions ?? d.thinking?.effort_options ?? d.effort_options ?? [])
      .filter((e) => typeof e === "string" && e && e !== "default" && e !== "none")
    out.push({
      id,
      name: str(d, "name") ?? id,
      context: num(d.limit?.context) ?? 0,
      output: num(d.limit?.output) ?? 0,
      images: input.includes("image") || d.attachment === true,
      thinking: mode === "switchable" ? "switchable"
        : mode === "forced_off" ? "off"
        : mode === "forced_on" ? "forced_on"
        : d.reasoning === false ? "off" : "on",
      efforts,
    })
  }
  return out
}

// A live authenticated model query has no anonymous or static fallback.
async function liveModels(site, access) {
  const url = new URL("/mavis/api/v1/models", site.llm)
  url.searchParams.set("region", site.region)
  url.searchParams.set("buildEnv", "prod")
  const res = await fetch(url, {
    headers: { Accept: "application/json", Authorization: "Bearer " + access },
    signal: AbortSignal.timeout(10000),
  })
  if (!res.ok) throw new Error(`${site.name}'s models: HTTP ${res.status}`)
  let body
  try { body = await res.json() } catch { throw new Error("MiniMax models returned invalid JSON") }
  return parseModels(body)
}

// ---- signing in (OAuth device flow) ---------------------------------------------

class OAuthError extends Error {
  constructor(code, status, message) {
    super(message ?? `MiniMax sign-in request failed${status ? ` (HTTP ${status})` : ""}`)
    this.code = code
    this.httpStatus = status
  }
}

// postForm posts an OAuth form, as MiniMax Code's client does; ok is false
// when the answer is an HTTP error or carries an "error".
async function postForm(url, fields) {
  const res = await fetch(url, {
    method: "POST",
    headers: { Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams(fields).toString(),
    signal: AbortSignal.timeout(30000),
  })
  let body
  try {
    body = obj(JSON.parse(await res.text()))
  } catch {}
  if (!body) throw new OAuthError("invalid_json_response", res.status)
  const error = str(body, "error")
  return { ok: res.ok && !error, status: res.status, body, error }
}

// token reads a token answer: refresh is the one to keep when none is sent.
function token(body, refresh) {
  const access = str(body, "access_token")
  const expiresIn = pos(body, "expires_in")
  if (!access || !expiresIn) throw new OAuthError("invalid_token_response")
  return {
    access,
    refresh: str(body, "refresh_token") ?? refresh ?? "",
    expires: Date.now() + expiresIn * 1000,
  }
}

async function startDevice(site) {
  const verifier = randomBytes(32).toString("base64url")
  const challenge = createHash("sha256").update(verifier, "ascii").digest("base64url")
  const r = await postForm(site.account + "/oauth2/device/code", {
    client_id: CLIENT_ID,
    scope: SCOPE,
    audience: AUDIENCE,
    code_challenge: challenge,
    code_challenge_method: "S256",
  })
  if (!r.ok) throw new OAuthError(r.error ?? "oauth_request_failed", r.status)
  const b = r.body
  const userCode = str(b, "user_code")
  const deviceCode = str(b, "device_code")
  const expiredIn = pos(b, "expired_in")
  // MiniMax's own variant: no device_code, the user code polled for, an
  // end that may be an at-ms, an interval in ms
  const byUser = !deviceCode && !!userCode && expiredIn !== undefined
  const uri = str(b, "verification_uri") ?? str(b, "verification_url")
  const at = expiredIn === undefined ? undefined : expiredIn < 1e12 ? expiredIn : Math.ceil((expiredIn - Date.now()) / 1000)
  const expiresIn = pos(b, "expires_in") ?? (at > 0 ? at : undefined)
  const interval = pos(b, "interval")
  if (!(deviceCode ?? (byUser ? userCode : undefined)) || !userCode || !uri) throw new OAuthError("invalid_device_authorization_response")
  const url = new URL(str(b, "verification_uri_complete") ?? uri)
  if (url.origin !== site.account || url.username || url.password || url.hash || url.pathname.length > 2048) throw new OAuthError("invalid_verification_url")
  return {
    verifier,
    userCode,
    poll: byUser ? { user_code: userCode } : { device_code: deviceCode },
    url: url.href,
    deadline: Date.now() + (expiresIn ? expiresIn * 1000 : SIGN_IN_MS),
    intervalMs: byUser && interval !== undefined ? interval : (interval ?? 5) * 1000,
  }
}

async function pollDevice(site, d) {
  let wait = d.intervalMs
  while (Date.now() < d.deadline) {
    await sleep(wait)
    const r = await postForm(site.account + "/oauth2/token", {
      grant_type: DEVICE_GRANT,
      ...d.poll,
      client_id: CLIENT_ID,
      code_verifier: d.verifier,
    })
    const status = str(r.body, "status")
    if (r.ok && status === "pending") continue
    if (r.ok && status === "slow_down") {
      wait += 5000
      continue
    }
    if (r.ok && (status === "denied" || status === "access_denied")) throw new OAuthError("access_denied", 0, "the sign-in was turned down")
    if (r.ok && (status === "expired" || status === "expired_token")) throw new OAuthError("expired_token", 0, "the sign-in code expired")
    if (r.ok) return token(r.body)
    if (r.error === "authorization_pending") continue
    if (r.error === "slow_down") {
      wait += 5000
      continue
    }
    throw new OAuthError(r.error ?? "device_authorization_failed", r.status)
  }
  throw new OAuthError("expired_token", 0, "the sign-in code expired")
}

// refreshToken trades a refresh token for a new access token.
async function refreshToken(site, refresh) {
  const r = await postForm(site.account + "/oauth2/token", {
    grant_type: "refresh_token",
    refresh_token: refresh,
    client_id: CLIENT_ID,
    scope: SCOPE,
    audience: AUDIENCE,
  })
  if (!r.ok) throw new OAuthError(r.error ?? "oauth_request_failed", r.status)
  return token(r.body, refresh)
}

// signedOut is the one answer that means the account is signed out, as
// MiniMax Code reads it: invalid_grant with HTTP 400. Other failures still
// stop this operation; the host owns unknown-rotation recovery.
const signedOut = (e) => e instanceof OAuthError && e.code === "invalid_grant" && e.httpStatus === 400

class SignInExpired extends Error {
  constructor(site) {
    super(`this ${site.name} account is signed out; sign in again`)
    this.signIn = "expired"
  }
}

// ---- MiniMax Code's signed account API -------------------------------------------

// signed is a request of MiniMax Code's account API, signed as mcode signs
// one: yy over the path and query, the body ("{}" for a GET) and the time;
// x-signature over the time (and the body of a POST).
function signed(site, path, { access, userID, body, origin = site.agent, now = Date.now(), tz = new Date().getTimezoneOffset() * -60, platform = process.platform } = {}) {
  const url = new URL(path, origin)
  url.search = new URLSearchParams({
    device_platform: "mcode", biz_id: "3", app_id: "3001", version_code: "22201",
    unix: String(now), timezone_offset: String(tz), sys_language: site.lang, lang: site.lang,
    device_id: "0", os_name: platform, browser_name: "mcode", user_id: String(userID ?? "").trim() || "0", client: "mcode",
  }).toString()
  const at = url.pathname + url.search
  const ts = Math.floor(now / 1000)
  const json = body === undefined ? undefined : JSON.stringify(body)
  const headers = {
    Accept: "application/json",
    "Content-Type": "application/json",
    "User-Agent": "MiniMaxCode",
    Authorization: "Bearer " + access,
    yy: json === undefined ? md5(`${encodeURIComponent(at)}_{}${md5(String(now))}ooui`) : md5(`${encodeURIComponent(at)}_${json}${md5(String(now))}ooui`),
    "x-timestamp": String(ts),
    "x-signature": json === undefined ? md5(`${ts}${SIGN_SECRET}`) : md5(`${ts}${SIGN_SECRET}${json}`),
  }
  return { url: url.toString(), init: { method: json === undefined ? "GET" : "POST", headers, ...(json === undefined ? {} : { body: json }) } }
}

class AccountRefused extends Error {}

// checked throws on an answer MiniMax's account API says failed in its body.
function checked(env, what) {
  const si = obj(env.statusInfo)
  if (typeof si?.code === "number" && si.code !== 0) {
    if (si.code === 1000048) throw new AccountRefused(`${what}: the sign-in was refused`)
    throw new Error(`${what}: upstream status ${si.code}`)
  }
  const br = obj(env.base_resp)
  if (typeof br?.status_code === "number" && br.status_code !== 0) throw new Error(`${what}: upstream status ${br.status_code}`)
  return env
}

async function accountCall(site, path, opts, what) {
  const { url, init } = signed(site, path, opts)
  const res = await fetch(url, { ...init, signal: AbortSignal.timeout(15000) })
  if (res.status === 401 || res.status === 403) throw new AccountRefused(`${what}: the sign-in was refused (HTTP ${res.status})`)
  if (!res.ok) throw new Error(`${what}: HTTP ${res.status}`)
  let env
  try {
    env = obj(JSON.parse(await res.text()))
  } catch {}
  if (!env) throw new Error(`${what}: not JSON`)
  return checked(env, what)
}

// Only the stable MiniMax user id is retained, never email or display name.
async function identity(site, access) {
  const env = await accountCall(site, "/v1/api/user/info", { access }, "account")
  const d = obj(env.data)
  const u = obj(d?.userInfo) ?? obj(d?.user_info) ?? obj(env.userInfo) ?? obj(env.user_info)
  const id = str(u, "realUserID") ?? str(u, "real_user_id")
  if (!id) throw new Error("account: no user id in the reply")
  return { realUserID: id }
}

// A worker serves one credential operation at a time; the Go host serializes
// rotations across workers. No in-memory credentials may bypass durable save.
function state(a) {
  if (a?.type !== "oauth" || !str(a, "access") || !str(a, "refresh") ||
      !pos(a, "expires") || !str(a, "uid") || a.uid !== a.realUserID ||
      typeof a.sessionID !== "string" || !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(a.sessionID)) {
    throw new Error("invalid MiniMax OAuth state")
  }
  return a
}
function makePlugin(site) {
  return async ({ client }) => {
    const renew = async (a) => {
      state(a)
      try {
        const t = await refreshToken(site, a.refresh)
        const next = { ...a, access: t.access, refresh: t.refresh, expires: t.expires }
        // Await host acknowledgement before any use of the rotated credentials.
        await client.auth.set({ path: { id: site.id }, body: next })
        return next
      } catch (e) {
        if (signedOut(e)) throw new SignInExpired(site)
        throw new Error(sanitize("MiniMax token refresh failed"))
      }
    }
    const fresh = async (getAuth) => {
      const a = state(await getAuth())
      if (Date.now() < a.expires - EARLY_MS) return { a, renewed: false }
      return { a: await renew(a), renewed: true }
    }
    return {
      config: async (config) => {
        config.provider ??= {}
        config.provider[site.id] = { name: site.name, npm: NPM, api: api(site), models: {} }
      },
      provider: {
        id: site.id,
        async models(provider, { auth }) {
          const { a } = await fresh(async () => auth)
          const ms = await liveModels(site, a.access)
          if (!ms.length) throw new Error("MiniMax returned no live models")
          return Object.fromEntries(ms.map(m => [m.id, modelOf(site, provider, m)]))
        },
      },
      auth: {
        provider: site.id,
        refreshLead: LEAD_MS,
        async refresh(auth) {
          await renew(state(auth))
          // renew already saved the whole state; host must not merge/save twice.
          return undefined
        },
        async loader(getAuth) {
          return {
            baseURL: api(site),
            apiKey: PLACEHOLDER_KEY,
            async fetch(input, init) {
              const req = input instanceof Request ? input : null
              const url = new URL(req ? req.url : input)
              if (url.href !== api(site) + "/messages" || (init?.method ?? req?.method ?? "POST") !== "POST") throw new Error("unsupported MiniMax inference endpoint")
              let body = init?.body
              if (body === undefined && req) body = await req.clone().text()
              if (body instanceof Uint8Array) body = new TextDecoder().decode(body)
              if (typeof body !== "string") throw new Error("MiniMax request must be JSON")
              let payload
              try { payload = JSON.parse(body) } catch { throw new Error("MiniMax request must be JSON") }
              validate(payload)
              const { a, renewed } = await fresh(getAuth)
              const headers = new Headers(init?.headers ?? req?.headers)
              headers.delete("content-length")
              headers.set("Content-Type", "application/json")
              headers.set("Authorization", "Bearer " + a.access)
              headers.set("x-api-key", PLACEHOLDER_KEY)
              headers.set("User-Agent", "MiniMaxAgent")
              headers.set("X-Mavis-Agent-Id", "main")
              headers.set("X-Mavis-Timezone-Offset", String(new Date().getTimezoneOffset() * -60))
              headers.set("X-Mavis-Session-Id", a.sessionID)
              if (!headers.has("anthropic-version")) headers.set("anthropic-version", "2023-06-01")
              // A 401 is returned once, never replayed as a second inference.
              const res = await fetch(url, { ...init, signal: init?.signal ?? req?.signal, method: "POST", headers, body })
              return answer(res, renewed, payload.stream === true)
            },
          }
        },
        methods: [{
          type: "oauth",
          label: `${site.name} account`,
          async authorize() {
            const d = await startDevice(site)
            return {
              url: d.url,
              user_code: d.userCode,
              instructions: `Sign in to ${site.name} in the browser and confirm the code ${d.userCode}; this finishes by itself.`,
              method: "auto",
              async callback() {
                const t = await pollDevice(site, d)
                const who = await identity(site, t.access)
                if (!t.refresh) throw new Error("MiniMax did not return a refresh token")
                return {
                  type: "success", access: t.access, refresh: t.refresh, expires: t.expires,
                  accountId: who.realUserID, uid: who.realUserID, realUserID: who.realUserID,
                  sessionID: randomUUID(),
                }
              },
            }
          },
        }],
      },
    }
  }
}

export const MiniMaxCodeAuthPlugin = makePlugin(SITES["minimax-code"])
export const MiniMaxCodeGlobalAuthPlugin = makePlugin(SITES["minimax-code-global"])
export const _torouter = { validate, setSanitizer }
