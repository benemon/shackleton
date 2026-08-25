import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let pages

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  pages = await vite.ssrLoadModule('/src/pages/Admin.tsx')
})

after(async () => {
  await vite.close()
})

const source = readFileSync(new URL('../src/pages/Admin.tsx', import.meta.url), 'utf8')

test('each admin page loads with its own header, blurb, and the read-only note', () => {
  for (const [name, title] of [
    ['AdminPlatform', 'Platform'],
    ['AdminTools', 'Tool servers'],
    ['AdminMetrics', 'Metrics sources'],
    ['AdminChannels', 'Channels'],
    ['AdminSweeps', 'Sweeps'],
  ]) {
    const markup = renderToStaticMarkup(React.createElement(MemoryRouter, null, React.createElement(pages[name])))
    assert.match(markup, new RegExp(`Administration / ${title}`), name)
    assert.match(markup, new RegExp(`>${title}<`), name)
    assert.match(markup, /This view is read-only\. Configuration comes from the daemon(?:&#x27;|')s config file\./, name)
    assert.match(markup, /pf-v6-c-spinner/, name)
  }
})

test('failure alerts derive from the page title and suppress the spinner', () => {
  assert.match(source, /<Alert variant="danger" title=\{`Could not load \$\{title\.toLowerCase\(\)\}`\}>/)
  assert.match(source, /\{loading \? error === '' && <PageLoading \/> : children\}/)
})

test('each admin surface pins its empty-state copy', () => {
  for (const sentence of [
    'No MCP servers configured.',
    'No tools require approval.',
    'No channels configured.',
    'No custom prompt configured.',
    'No knowledge sources configured.',
  ]) {
    assert.ok(source.includes(sentence), sentence)
  }
  assert.match(source, /titleText="No sources configured"/)
  assert.match(source, /titleText="No sweeps configured"/)
})
