import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let Investigations
let EventStream
let GatedActions
let stripVerdictBlock

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  ;({ Investigations, EventStream, GatedActions } = await vite.ssrLoadModule('/src/pages/Investigations.tsx'))
  ;({ stripVerdictBlock } = await vite.ssrLoadModule('/src/utils.ts'))
})

after(async () => {
  await vite.close()
})

const source = readFileSync(new URL('../src/pages/Investigations.tsx', import.meta.url), 'utf8')

// Mirrors #/components/schemas/Event in api/openapi.yaml: an
// approval_requested/approval_decided pair sharing call_id, plus a tool call.
const events = [
  {
    ts: '2026-08-24T10:00:00Z',
    type: 'tool_call',
    payload: { round: 1, name: 'query_metrics', args: { query: 'up' }, result_snippet: '1', error: false },
  },
  {
    ts: '2026-08-24T10:00:05Z',
    type: 'approval_requested',
    payload: { call_id: 'call-1', name: 'run_host_command', human: 'Restart chronyd on nas' },
  },
  {
    ts: '2026-08-24T10:01:00Z',
    type: 'approval_decided',
    payload: { call_id: 'call-1', approved: false, via: 'telegram' },
  },
  {
    ts: '2026-08-24T10:02:00Z',
    type: 'approval_requested',
    payload: { call_id: 'call-2', name: 'run_kubectl_mutation', human: 'Delete the stuck CSV' },
  },
]

test('while loading the list shows a spinner and zeroed tabs, never a fake table', () => {
  const markup = renderToStaticMarkup(React.createElement(MemoryRouter, null, React.createElement(Investigations)))
  assert.match(markup, />Investigations</)
  assert.match(markup, /Questions \(0\)/)
  assert.match(markup, /Alerts \(0\)/)
  assert.match(markup, /Sweeps \(0\)/)
  assert.match(markup, /pf-v6-c-spinner/)
  assert.doesNotMatch(markup, /aria-label="questions investigations"/)
})

test('the event stream renders each event with its type label and detail', () => {
  const markup = renderToStaticMarkup(React.createElement(EventStream, { events }))
  assert.match(markup, /Event stream/)
  assert.match(markup, /query_metrics \{&quot;query&quot;:&quot;up&quot;\}/)
  assert.match(markup, /→ 1/)
  assert.match(markup, /Restart chronyd on nas/)
  assert.match(markup, /denied via telegram/)
})

test('an empty event stream says so', () => {
  const markup = renderToStaticMarkup(React.createElement(EventStream, { events: [] }))
  assert.match(markup, /No events have been recorded yet\./)
})

test('gated actions pair each request with its decision by call id', () => {
  const markup = renderToStaticMarkup(React.createElement(GatedActions, { events }))
  assert.match(markup, /run_host_command/)
  assert.match(markup, />denied</)
  assert.match(markup, /via telegram/)
  // call-2 has no decision yet.
  assert.match(markup, />awaiting</)
  assert.doesNotMatch(markup, />approved</)
})

test('no gated actions renders the honest empty sentence', () => {
  const markup = renderToStaticMarkup(React.createElement(GatedActions, { events: [] }))
  assert.match(markup, /No gated actions were proposed\./)
})

test('the detail page pins its verdict placeholders and failure alert', () => {
  for (const sentence of [
    'This investigation is still running; a verdict has not been written yet.',
    'The investigation failed before producing a verdict',
    'This investigation completed before structured verdicts were recorded.',
    'No answer was recorded.',
  ]) {
    assert.ok(source.includes(sentence), sentence)
  }
  assert.match(source, /<Alert variant="danger" title="Could not load the investigation">/)
})

test('the list pins its empty and failure copy', () => {
  assert.match(source, /titleText="No investigations match these filters"/)
  assert.match(source, /tab === 'questions' \? 'No questions yet' : tab === 'alerts' \? 'No alerts yet' : 'No sweeps yet'/)
  assert.match(source, /<Alert variant="danger" title="Could not load investigations">/)
  assert.match(source, /error === '' && <PageLoading \/>/)
})

test('tabs split by trigger and rows link through the derived title', () => {
  // Questions exclude both symptomatic prefixes; alerts and sweeps match theirs exactly.
  assert.match(source, /!item\.trigger\.startsWith\('sweep:'\) && !item\.trigger\.startsWith\('alert:'\)/)
  assert.match(source, /item\.trigger\.startsWith\('alert:'\)/)
  assert.match(source, /\{item\.title\}/)
  assert.doesNotMatch(source, /\{item\.question\}/)
})

test('the detail header uses the title and keeps the question readable in full', () => {
  assert.match(source, /title=\{summary\.title\}/)
  assert.match(source, /<pre className="mono-pre">\{summary\.question\}<\/pre>/)
  assert.match(source, /toggleTextCollapsed="Show the question as written"/)
})

test('the answer panel renders markdown after stripping a parsed verdict block', () => {
  assert.match(source, /<Markdown source=\{stripVerdictBlock\(summary\.answer, summary\.verdict !== undefined\)\} \/>/)
  const answer = '# Result\n```json\n{"verdict":"healthy"}\n```'
  assert.equal(stripVerdictBlock(answer, true), '# Result\n')
  assert.equal(stripVerdictBlock(answer, false), answer)
  // A JSON block quoted mid-answer or without verdict content is prose, not plumbing.
  const quoted = 'see:\n```json\n{"verdict":"healthy"}\n```\nmore prose after'
  assert.equal(stripVerdictBlock(quoted, true), quoted)
  const config = 'x\n```json\n{"replicas": 3}\n```'
  assert.equal(stripVerdictBlock(config, true), config)
})
