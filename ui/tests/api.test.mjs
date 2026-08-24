import assert from 'node:assert/strict'
import { after, before, beforeEach, test } from 'node:test'

import { createServer } from 'vite'

let vite
let api
let APIError
let streamSSE

const storage = new Map()
globalThis.sessionStorage = {
  getItem: (key) => (storage.has(key) ? storage.get(key) : null),
  setItem: (key, value) => storage.set(key, String(value)),
  removeItem: (key) => storage.delete(key),
}
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
  ;({ api, APIError, streamSSE } = await vite.ssrLoadModule('/src/api.ts'))
})

after(async () => {
  await vite.close()
})

beforeEach(() => {
  storage.clear()
  reloads = 0
})

test('requests carry the stored token as a bearer header', async () => {
  storage.set('shackleton-token', 'secret')
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

test('a 401 clears the stored token and reloads back to the gate', async () => {
  storage.set('shackleton-token', 'stale')
  globalThis.fetch = async () => jsonResponse(401, { error: 'unauthorized' })
  await assert.rejects(() => api.getHealth())
  assert.equal(storage.has('shackleton-token'), false)
  assert.equal(reloads, 1)
})

test('a non-401 failure surfaces the daemon error without touching the session', async () => {
  storage.set('shackleton-token', 'secret')
  globalThis.fetch = async () => jsonResponse(500, { error: 'store unavailable' })
  await assert.rejects(
    () => api.getHealth(),
    (reason) => reason instanceof APIError && reason.status === 500 && reason.message === 'store unavailable',
  )
  assert.equal(storage.get('shackleton-token'), 'secret')
  assert.equal(reloads, 0)
})

test('the raw-markdown article fetch shares the 401 gate behaviour', async () => {
  storage.set('shackleton-token', 'stale')
  globalThis.fetch = async () => jsonResponse(401, { error: 'unauthorized' })
  await assert.rejects(() => api.getKBArticle('slug'))
  assert.equal(storage.has('shackleton-token'), false)
  assert.equal(reloads, 1)

  storage.set('shackleton-token', 'secret')
  globalThis.fetch = async () => ({ ok: true, status: 200, text: async () => '# article' })
  assert.equal(await api.getKBArticle('slug'), '# article')
})

test('the SSE parser assembles frames across chunk boundaries', async () => {
  storage.set('shackleton-token', 'secret')
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
