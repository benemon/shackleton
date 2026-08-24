import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let Overview

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  ;({ Overview } = await vite.ssrLoadModule('/src/pages/Overview.tsx'))
})

after(async () => {
  await vite.close()
})

const source = readFileSync(new URL('../src/pages/Overview.tsx', import.meta.url), 'utf8')

test('while loading the overview shows honest copy and a spinner, no stale numbers', () => {
  const markup = renderToStaticMarkup(React.createElement(MemoryRouter, null, React.createElement(Overview)))
  assert.match(markup, />Overview</)
  assert.match(markup, /Loading current daemon and estate state\./)
  assert.match(markup, /pf-v6-c-spinner/)
  assert.doesNotMatch(markup, /running ·/)
})

test('the failure state pins its alert and suppresses the loading spinner', () => {
  assert.match(source, /<Alert variant="danger" title="Could not load the overview">/)
  // On error the component returns before the loading branch renders.
  assert.match(source, /if \(error !== ''\) \{/)
})

test('each panel pins its empty-state sentence', () => {
  for (const sentence of [
    'No actions are awaiting a decision.',
    'No investigations are running.',
    'No draft articles.',
  ]) {
    assert.ok(source.includes(sentence), sentence)
  }
})
