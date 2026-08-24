import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'

import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { createServer } from 'vite'

let vite
let Inventory
let InventoryPanels

before(async () => {
  vite = await createServer({
    root: process.cwd(),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: { noExternal: [/@patternfly\//] },
  })
  ;({ Inventory, InventoryPanels } = await vite.ssrLoadModule('/src/pages/Inventory.tsx'))
})

after(async () => {
  await vite.close()
})

const source = readFileSync(new URL('../src/pages/Inventory.tsx', import.meta.url), 'utf8')

// Mirrors #/components/schemas/Inventory in api/openapi.yaml.
const inventory = {
  hosts: [
    { name: 'nas', hostname: 'nas.lab.example', connection: 'ssh' },
    { name: 'mini', hostname: 'mini.lab.example', connection: 'ssh', status: 'approved' },
    { name: 'attic', connection: 'ssh', status: 'draft', source: 'prometheus', first_seen: '2026-08-20T10:00:00Z' },
    { name: 'printer', connection: 'ssh', status: 'ignored', source: 'prometheus', first_seen: '2026-08-19T10:00:00Z' },
    { name: 'node1', connection: 'ssh', cluster: 'ocp', source: 'kube-state-metrics', first_seen: '2026-08-21T10:00:00Z' },
  ],
  clusters: [{ name: 'ocp', api: 'https://api.ocp.lab.example:6443', type: 'openshift' }],
}

const renderPanels = (data) => renderToStaticMarkup(React.createElement(
  MemoryRouter,
  null,
  React.createElement(InventoryPanels, { inventory: data, expanded: {}, setExpanded: () => {} }),
))

test('while loading the inventory shows only its header and a spinner', () => {
  const markup = renderToStaticMarkup(React.createElement(MemoryRouter, null, React.createElement(Inventory)))
  assert.match(markup, />Inventory</)
  assert.match(markup, /pf-v6-c-spinner/)
  assert.doesNotMatch(markup, /aria-label="Hosts"/)
})

test('the hosts tab renders standalone hosts with honest state labels', () => {
  const markup = renderPanels(inventory)
  // Cluster members never appear as standalone host rows.
  assert.match(markup, /Hosts \(4\)/)
  assert.match(markup, /Clusters \(1\)/)
  assert.match(markup, /4 hosts · 2 actionable/)
  assert.doesNotMatch(markup, />node1</)
  assert.match(markup, /draft · inert/)
  assert.match(markup, /ignored · inert/)
  assert.match(markup, />declared</)
  assert.match(markup, />approved</)
  // Inert rows are visually distinguished, never hidden.
  assert.equal((markup.match(/inert-row/g) ?? []).length, 2)
})

test('an empty inventory renders the empty state, not a bare table', () => {
  const markup = renderPanels({ hosts: [], clusters: [] })
  assert.match(markup, /No hosts match these filters/)
  assert.doesNotMatch(markup, /aria-label="Hosts"/)
})

test('failure and cluster empty states pin their copy', () => {
  assert.match(source, /<Alert variant="danger" title="Could not load inventory">/)
  assert.match(source, /error === '' && <PageLoading \/>/)
  assert.match(source, /titleText="No clusters"/)
  assert.ok(source.includes('No discovered members.'))
  assert.ok(source.includes('cluster member · inert'))
})
