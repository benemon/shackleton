import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let KB
let Markdown

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  ;({ KB, Markdown } = await vite.ssrLoadModule('/src/pages/KB.tsx'))
})

after(async () => {
  await vite.close()
})

const source = readFileSync(new URL('../src/pages/KB.tsx', import.meta.url), 'utf8')

test('while loading the list shows a spinner and no filter toolbar', () => {
  const markup = renderToStaticMarkup(React.createElement(MemoryRouter, null, React.createElement(KB)))
  assert.match(markup, /Knowledge base/)
  assert.match(markup, /pf-v6-c-spinner/)
  // The toolbar only renders once articles exist — it must never offer
  // filters over data that is not there.
  assert.doesNotMatch(markup, /Filter by title or symptom/)
})

test('filter options are derived from the loaded articles, never a fixed list', () => {
  assert.match(source, /\['approved', 'draft', 'nominated'\]\.filter\(\(state\) =>\s*\n\s*loaded\.some/)
  assert.match(source, /\['healthy', 'attention', 'action', 'none'\]\.filter\(\(verdict\) =>\s*\n\s*loaded\.some/)
})

test('the list pins its empty and failure copy', () => {
  assert.match(source, /titleText="No articles yet"/)
  assert.ok(source.includes('Articles are drafted from investigations that end in a verdict.'))
  assert.match(source, /titleText="No articles match these filters"/)
  assert.match(source, /<Alert variant="danger" title="Could not load the knowledge base">/)
  assert.match(source, /error === '' && <PageLoading \/>/)
})

test('the article page pins its resolution sentences and failure alert', () => {
  for (const sentence of [
    'The recorded resolution cleared the symptom.',
    'The symptom persisted after the recorded resolution.',
    'The recorded resolution has not been verified yet.',
    'No linked investigations.',
  ]) {
    assert.ok(source.includes(sentence), sentence)
  }
  assert.match(source, /<Alert variant="danger" title="Could not load the article">/)
})

test('the markdown renderer handles front matter, headings, fences and lists', () => {
  const markup = renderToStaticMarkup(React.createElement(Markdown, {
    source: [
      '---', 'slug: alert-chrony', '---',
      '# Clock drift on nas', '', 'Chrony lost sync.', '## Remediation',
      '1. Restart chronyd', '- verify offset', '```', 'chronyc tracking', '```',
    ].join('\n'),
  }))
  assert.doesNotMatch(markup, /slug: alert-chrony/)
  assert.match(markup, /<h1>Clock drift on nas<\/h1>/)
  assert.match(markup, /<h2>Remediation<\/h2>/)
  assert.match(markup, /<p>Chrony lost sync\.<\/p>/)
  assert.match(markup, />1\.<\/span><span>Restart chronyd</)
  assert.match(markup, />—<\/span><span>verify offset</)
  assert.match(markup, /<pre class="mono-pre">chronyc tracking<\/pre>/)
})

test('an unterminated fence still renders its content', () => {
  const markup = renderToStaticMarkup(React.createElement(Markdown, { source: '```\ndangling' }))
  assert.match(markup, /<pre class="mono-pre">dangling<\/pre>/)
})
