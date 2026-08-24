import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let App
let TokenGate
let Console

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  ;({ App, TokenGate, Console } = await vite.ssrLoadModule('/src/App.tsx'))
})

after(async () => {
  await vite.close()
})

const render = (element, pathname = '/') => renderToStaticMarkup(React.createElement(
  MemoryRouter,
  { initialEntries: [pathname] },
  element,
))

const shellSource = readFileSync(new URL('../src/App.tsx', import.meta.url), 'utf8')

test('boot pairs a spinner with visible restoring copy and shows neither gate nor console', () => {
  const markup = render(React.createElement(App))
  assert.match(markup, /Restoring your session…/)
  assert.match(markup, /pf-v6-c-spinner/)
  assert.doesNotMatch(markup, /Connect to the daemon console/)
  assert.doesNotMatch(markup, /Console navigation/)
})

test('the gate renders a password field and stores the token in memory plus the session cookie', () => {
  const markup = render(React.createElement(TokenGate, { onSet: () => {} }))
  assert.match(markup, /Connect to the daemon console\./)
  assert.match(markup, /<input[^>]*id="token"[^>]*type="password"/)
  assert.match(markup, />Connect</)
  // Sign-in POSTs the fresh token fire-and-forget (ADR-0021 shape); the token
  // itself never touches web storage.
  assert.match(shellSource, /session\.create\(token\)\.catch/)
  assert.doesNotMatch(shellSource, /sessionStorage/)
})

test('sign-out deletes the session before dropping the in-memory token', () => {
  assert.match(shellSource, /session\.end\(\)\.finally/)
})

// The exact shipped navigation. A slice that adds or renames a nav entry
// plans this fixture update in (Dufflebag smoke convention).
const primaryLabels = ['Overview', 'Investigations', 'Approvals', 'Knowledge base', 'Inventory']
const administrationLabels = ['Platform', 'Tool servers', 'Metrics sources', 'Channels', 'Sweeps']

test('the console renders the exact nav item list', () => {
  const markup = render(React.createElement(Console))
  for (const label of [...primaryLabels, ...administrationLabels, 'Administration']) {
    assert.match(markup, new RegExp(`>${label}<`), label)
  }
  // 10 entries plus the Administration expandable itself.
  assert.equal((markup.match(/class="pf-v6-c-nav__item[ "]/g) ?? []).length, 11)

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
  const markup = render(React.createElement(Console), '/investigations')
  assert.match(markup, /pf-v6-c-nav__link pf-m-current[^"]*" aria-current="page">Investigations</)
  assert.equal((markup.match(/aria-current="page"/g) ?? []).length, 1)
})

test('before health arrives the header reports connecting, never a fake status', () => {
  const markup = render(React.createElement(Console))
  assert.match(markup, /connecting · loading model/)
  assert.match(markup, />Disconnect</)
  assert.doesNotMatch(markup, /status-dot--ok/)
})
