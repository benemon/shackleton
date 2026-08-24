import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let Approvals

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  ;({ Approvals } = await vite.ssrLoadModule('/src/pages/Approvals.tsx'))
})

after(async () => {
  await vite.close()
})

const source = readFileSync(new URL('../src/pages/Approvals.tsx', import.meta.url), 'utf8')

test('while loading the approvals page shows only its header and a spinner', () => {
  const markup = renderToStaticMarkup(React.createElement(MemoryRouter, null, React.createElement(Approvals)))
  assert.match(markup, />Approvals</)
  assert.match(markup, /pf-v6-c-spinner/)
  assert.doesNotMatch(markup, /Nothing pending/)
  assert.doesNotMatch(markup, /awaiting/)
})

test('the empty state pins its exact copy', () => {
  assert.match(source, /titleText="Nothing pending"/)
  assert.ok(source.includes('Gated actions will appear here when an investigation needs permission to continue.'))
})

test('failure and lost-race states pin their alerts', () => {
  assert.match(source, /<Alert variant="danger" title="Could not load approvals">/)
  assert.ok(source.includes('Already decided elsewhere — the decision that settled first stands'))
  // A 409 means the other channel settled first: refresh, never surface an error.
  assert.match(source, /reason instanceof APIError && reason\.status === 409/)
  // The spinner never renders on top of an error.
  assert.match(source, /error === '' && <PageLoading \/>/)
})

test('deciding always passes through the typed confirm modal', () => {
  assert.match(source, /setModal\(\{ item, approved: true \}\)/)
  assert.match(source, /setModal\(\{ item, approved: false \}\)/)
  assert.match(source, /onClick=\{decide\}/)
})
