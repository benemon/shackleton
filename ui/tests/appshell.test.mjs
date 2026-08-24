import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let App

const storage = new Map()
globalThis.sessionStorage = {
  getItem: (key) => (storage.has(key) ? storage.get(key) : null),
  setItem: (key, value) => storage.set(key, String(value)),
  removeItem: (key) => storage.delete(key),
}

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  ;({ App } = await vite.ssrLoadModule('/src/App.tsx'))
})

after(async () => {
  await vite.close()
})

const render = (pathname = '/') => renderToStaticMarkup(React.createElement(
  MemoryRouter,
  { initialEntries: [pathname] },
  React.createElement(App),
))

test('without a token the gate renders and the console does not', () => {
  storage.clear()
  const markup = render()
  assert.match(markup, /Connect to the daemon console\./)
  assert.match(markup, /<input[^>]*id="token"[^>]*type="password"/)
  assert.match(markup, />Connect</)
  assert.doesNotMatch(markup, /Console navigation/)
  assert.doesNotMatch(markup, /Disconnect/)
})

// The exact shipped navigation. A slice that adds or renames a nav entry
// plans this fixture update in (Dufflebag smoke convention).
const primaryLabels = ['Overview', 'Investigations', 'Approvals', 'Knowledge base', 'Inventory']
const administrationLabels = ['Platform', 'Tool servers', 'Metrics sources', 'Channels', 'Sweeps']

test('with a token the console renders the exact nav item list', () => {
  storage.set('shackleton-token', 'secret')
  const markup = render()
  for (const label of [...primaryLabels, ...administrationLabels, 'Administration']) {
    assert.match(markup, new RegExp(`>${label}<`), label)
  }
  // 10 entries plus the Administration expandable itself.
  assert.equal((markup.match(/class="pf-v6-c-nav__item[ "]/g) ?? []).length, 11)

  const shellSource = readFileSync(new URL('../src/App.tsx', import.meta.url), 'utf8')
  for (const [label, path] of [
    ['Overview', '/'], ['Investigations', '/investigations'], ['Approvals', '/approvals'],
    ['Knowledge base', '/kb'], ['Inventory', '/inventory'],
    ['Platform', '/admin/platform'], ['Tool servers', '/admin/tools'], ['Metrics sources', '/admin/metrics'],
    ['Channels', '/admin/channels'], ['Sweeps', '/admin/sweeps'],
  ]) {
    assert.ok(shellSource.includes(`path: '${path}', label: '${label}'`), `${path} → ${label}`)
  }
})

test('the current route marks exactly its own nav item current', () => {
  storage.set('shackleton-token', 'secret')
  const markup = render('/investigations')
  assert.match(markup, /pf-v6-c-nav__link pf-m-current[^"]*" aria-current="page">Investigations</)
  assert.equal((markup.match(/aria-current="page"/g) ?? []).length, 1)
})

test('before health arrives the header reports connecting, never a fake status', () => {
  storage.set('shackleton-token', 'secret')
  const markup = render()
  assert.match(markup, /connecting · loading model/)
  assert.match(markup, />Disconnect</)
  assert.doesNotMatch(markup, /status-dot--ok/)
})
