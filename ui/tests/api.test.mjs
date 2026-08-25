import assert from 'node:assert/strict'
import { after, before, beforeEach, test } from 'node:test'

import { createServer } from 'vite'

let vite
let api
let APIError
let streamSSE
let session
let getToken
let setToken
let clearToken

let reloads = 0
globalThis.window = { location: { reload: () => { reloads += 1 } } }

const jsonResponse = (status, body) => ({
  ok: status >= 200 && status < 300,
  status,
  statusText: `status ${status}`,
  json: async () => body,
  text: async () => JSON.stringify(body),
})

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  })
  ;({ api, APIError, streamSSE, session, getToken, setToken, clearToken } =
    await vite.ssrLoadModule('/src/api.ts'))
})

after(async () => {
  await vite.close()
})

beforeEach(() => {
  clearToken()
  reloads = 0
})

test('requests carry the in-memory token as a bearer header', async () => {
  setToken('secret')
  let seen
  globalThis.fetch = async (path, init) => {
    seen = { path, headers: init.headers }
    return jsonResponse(200, { status: 'ok' })
  }
  const health = await api.getHealth()
  assert.deepEqual(health, { status: 'ok' })
  assert.equal(seen.path, '/v1/health')
  assert.equal(seen.headers.Authorization, 'Bearer secret')
})

test('saving an investigation posts to its encoded KB route', async () => {
  setToken('secret')
  let seen
  globalThis.fetch = async (path, init) => {
    seen = { path, init }
    return jsonResponse(201, { slug: 'draft' })
  }
  assert.deepEqual(await api.saveInvestigationToKB('id/with slash'), { slug: 'draft' })
  assert.equal(seen.path, '/v1/investigations/id%2Fwith%20slash/kb')
  assert.equal(seen.init.method, 'POST')
  assert.equal(seen.init.headers.Authorization, 'Bearer secret')
})

test('a 401 drops the in-memory token and reloads back to the boot exchange', async () => {
  setToken('stale')
  globalThis.fetch = async () => jsonResponse(401, { error: 'unauthorized' })
  await assert.rejects(() => api.getHealth())
  assert.equal(getToken(), null)
  assert.equal(reloads, 1)
})

test('a non-401 failure surfaces the daemon error without touching the session', async () => {
  setToken('secret')
  globalThis.fetch = async () => jsonResponse(500, { error: 'store unavailable' })
  await assert.rejects(
    () => api.getHealth(),
    (reason) => reason instanceof APIError && reason.status === 500 && reason.message === 'store unavailable',
  )
  assert.equal(getToken(), 'secret')
  assert.equal(reloads, 0)
})

test('the raw-markdown article fetch shares the 401 gate behaviour', async () => {
  setToken('stale')
  globalThis.fetch = async () => jsonResponse(401, { error: 'unauthorized' })
  await assert.rejects(() => api.getKBArticle('slug'))
  assert.equal(getToken(), null)
  assert.equal(reloads, 1)

  setToken('secret')
  globalThis.fetch = async () => ({ ok: true, status: 200, text: async () => '# article' })
  assert.equal(await api.getKBArticle('slug'), '# article')
})

test('restore exchanges the cookie for a token on 200 and yields null on 204', async () => {
  globalThis.fetch = async (path, init) => {
    assert.equal(path, '/v1/session')
    assert.equal(init, undefined)
    return jsonResponse(200, { token: 'restored' })
  }
  assert.equal(await session.restore(), 'restored')

  globalThis.fetch = async () => ({ ok: true, status: 204 })
  assert.equal(await session.restore(), null)
})

test('creating a session presents the fresh token as a bearer, ending it needs none', async () => {
  const calls = []
  globalThis.fetch = async (path, init) => {
    calls.push({ path, init })
    return { ok: true, status: 204 }
  }
  await session.create('fresh')
  await session.end()
  assert.equal(calls[0].path, '/v1/session')
  assert.equal(calls[0].init.method, 'POST')
  assert.equal(calls[0].init.headers.Authorization, 'Bearer fresh')
  assert.equal(calls[1].init.method, 'DELETE')
  assert.equal(calls[1].init.headers, undefined)
})

test('the SSE parser assembles frames across chunk boundaries', async () => {
  setToken('secret')
  const chunks = [
    'event: requested\ndata: {"a"',
    ':1}\n\ndata: plain\n\nevent: settled\nda',
    'ta: {"b":2}\n\n',
  ].map((chunk) => new TextEncoder().encode(chunk))
  globalThis.fetch = async () => ({
    ok: true,
    status: 200,
    body: {
      getReader: () => ({
        read: async () =>
          chunks.length > 0 ? { done: false, value: chunks.shift() } : { done: true, value: undefined },
      }),
    },
  })
  const events = []
  await streamSSE('/v1/approvals/events', (type, data) => events.push([type, data]), new AbortController().signal)
  assert.deepEqual(events, [
    ['requested', '{"a":1}'],
    ['message', 'plain'],
    ['settled', '{"b":2}'],
  ])
})
