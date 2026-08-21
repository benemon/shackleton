import { useEffect, useState, type ReactNode } from 'react';
import {
  Alert,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  EmptyState,
  EmptyStateBody,
  ExpandableSection,
  Label,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { api, type AuditEntry, type ConfigView, type Summary } from '../api';
import { PageHeader, PageLoading, Panel, PanelHeader, SecretRefView, VerdictLabel } from '../components';
import { relativeTime, secretRefText } from '../utils';

const blurbs = {
  platform:
    'The daemon itself: where it listens, what it stores, which model it reasons with, and how long it is allowed to think.',
  tools: 'Configured MCP endpoints and the tools that require an operator decision.',
  metrics: 'The metrics and logs endpoints available to investigations.',
  channels: 'Where the daemon speaks, and where an approval can be settled from outside this console.',
  sweeps: 'Scheduled questions. Read each question in full when reviewing the verdicts it produces.',
};

function AdminHeader({ title, blurb }: { title: string; blurb: string }) {
  return (
    <>
      <PageHeader title={title} subtitle={blurb} eyebrow={`Administration / ${title}`} />
      <div className="admin-note subtle">This view is read-only. Configuration comes from the daemon&apos;s config file.</div>
    </>
  );
}

function AdminState({
  title,
  blurb,
  error,
  loading,
  children,
}: {
  title: string;
  blurb: string;
  error: string;
  loading: boolean;
  children: ReactNode;
}) {
  return (
    <div className="page">
      <AdminHeader title={title} blurb={blurb} />
      {error !== '' && (
        <Alert variant="danger" title={`Could not load ${title.toLowerCase()}`}>
          {error}
        </Alert>
      )}
      {loading ? error === '' && <PageLoading /> : children}
    </div>
  );
}

function useConfig() {
  const [config, setConfig] = useState<ConfigView | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    api.getConfig().then(setConfig, (reason) => setError(String(reason)));
  }, []);
  return { config, error };
}

export function AdminPlatform() {
  const { config, error } = useConfig();
  return (
    <AdminState title="Platform" blurb={blurbs.platform} error={error} loading={config === null}>
      {config !== null && (
        <>
          <div className="description-grid">
            <Panel>
              <PanelHeader>Daemon</PanelHeader>
              <div className="panel__body">
                <DescriptionList isHorizontal isCompact>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Listen</DescriptionListTerm>
                    <DescriptionListDescription><span className="mono mono--small">{config.listen || '—'}</span></DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>State dir</DescriptionListTerm>
                    <DescriptionListDescription><span className="mono mono--small">{config.state_dir}</span></DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Inventory dir</DescriptionListTerm>
                    <DescriptionListDescription><span className="mono mono--small">{config.inventory_dir}</span></DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Env files</DescriptionListTerm>
                    <DescriptionListDescription>
                      <span className="mono mono--small">{config.env_files.join(', ') || '—'}</span>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>API token</DescriptionListTerm>
                    <DescriptionListDescription><SecretRefView secret={config.api_token} /></DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </div>
            </Panel>

            <Panel>
              <PanelHeader>Transport</PanelHeader>
              <div className="panel__body">
                <DescriptionList isHorizontal isCompact>
                  <DescriptionListGroup>
                    <DescriptionListTerm>TLS certificate</DescriptionListTerm>
                    <DescriptionListDescription>
                      <span className="inline-cluster">
                        <span className="mono mono--small">{config.tls.cert_file || '—'}</span>
                        {config.tls.cert_file !== '' && config.tls.cert_file === config.tls.key_file && (
                          <Label color="grey" isCompact>same bundle</Label>
                        )}
                      </span>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>TLS key</DescriptionListTerm>
                    <DescriptionListDescription><span className="mono mono--small">{config.tls.key_file || '—'}</span></DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </div>
            </Panel>

            <Panel>
              <PanelHeader>Model</PanelHeader>
              <div className="panel__body">
                <DescriptionList isHorizontal isCompact>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Base URL</DescriptionListTerm>
                    <DescriptionListDescription><span className="mono mono--small">{config.model.base_url}</span></DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Name</DescriptionListTerm>
                    <DescriptionListDescription><span className="mono">{config.model.name}</span></DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>API key</DescriptionListTerm>
                    <DescriptionListDescription><SecretRefView secret={config.model.api_key} /></DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </div>
            </Panel>

            <Panel>
              <PanelHeader>Agent limits</PanelHeader>
              <div className="panel__body">
                <DescriptionList isHorizontal isCompact>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Max rounds</DescriptionListTerm>
                    <DescriptionListDescription>{config.agent.max_rounds}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Max tool result chars</DescriptionListTerm>
                    <DescriptionListDescription>{config.agent.max_tool_result_chars}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Call timeout</DescriptionListTerm>
                    <DescriptionListDescription>{config.agent.call_timeout}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Investigation timeout</DescriptionListTerm>
                    <DescriptionListDescription>{config.agent.investigation_timeout}</DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </div>
            </Panel>
          </div>

          <Panel>
            <PanelHeader>System prompt</PanelHeader>
            <div className="panel__body">
              <ExpandableSection toggleTextCollapsed="Show prompt" toggleTextExpanded="Hide prompt">
                <pre className="mono-pre">{config.agent.prompt || 'No custom prompt configured.'}</pre>
              </ExpandableSection>
            </div>
          </Panel>
        </>
      )}
    </AdminState>
  );
}

function approvalRequestCount(tool: string, audit: AuditEntry[]): number {
  const cutoff = Date.now() - 30 * 24 * 60 * 60 * 1000;
  return audit.filter(
    (entry) =>
      entry.type === 'approval_requested' &&
      new Date(entry.ts).getTime() >= cutoff &&
      String((entry.payload as Record<string, unknown>).name ?? '') === tool,
  ).length;
}

export function AdminTools() {
  const [data, setData] = useState<{ config: ConfigView; audit: AuditEntry[] } | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    Promise.all([api.getConfig(), api.getAudit()]).then(
      ([config, audit]) => setData({ config, audit }),
      (reason) => setError(String(reason)),
    );
  }, []);
  return (
    <AdminState title="Tool servers" blurb={blurbs.tools} error={error} loading={data === null}>
      {data !== null && (
        <>
          <Panel>
            <PanelHeader>MCP servers</PanelHeader>
            {data.config.mcp_servers.length === 0 ? (
              <div className="panel__body">No MCP servers configured.</div>
            ) : (
              <Table variant="compact" aria-label="MCP servers">
                <Thead><Tr><Th>Name</Th><Th>URL</Th></Tr></Thead>
                <Tbody>
                  {data.config.mcp_servers.map((server) => (
                    <Tr key={server.name}>
                      <Td dataLabel="Name"><span className="mono">{server.name}</span></Td>
                      <Td dataLabel="URL"><span className="mono mono--small">{server.url}</span></Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            )}
          </Panel>
          <Panel>
            <PanelHeader>Gated tools</PanelHeader>
            <div className="panel__body">
              {data.config.gated_tools.length === 0 ? (
                <span className="subtle">No tools require approval.</span>
              ) : (
                data.config.gated_tools.map((tool) => {
                  const count = approvalRequestCount(tool, data.audit);
                  return (
                    <div className="item-row" key={tool}>
                      <Label color="orangered" isCompact>gated</Label>
                      <span className="mono">{tool}</span>
                      <span className="subtle" style={{ marginLeft: 'auto' }}>
                        {count} approval {count === 1 ? 'request' : 'requests'} in the last 30 days
                      </span>
                    </div>
                  );
                })
              )}
            </div>
          </Panel>
        </>
      )}
    </AdminState>
  );
}

export function AdminMetrics() {
  const { config, error } = useConfig();
  const sources = config === null ? [] : [...config.metrics_sources, ...config.logs_sources];
  return (
    <AdminState title="Metrics sources" blurb={blurbs.metrics} error={error} loading={config === null}>
      {config !== null && (
        <Panel>
          <PanelHeader>Metrics and logs sources</PanelHeader>
          {sources.length === 0 ? (
            <EmptyState titleText="No sources configured" headingLevel="h2" variant="sm">
              <EmptyStateBody>Metrics and logs endpoints will appear here when configured.</EmptyStateBody>
            </EmptyState>
          ) : (
            <Table variant="compact" aria-label="Metrics and logs sources">
              <Thead><Tr><Th>Name</Th><Th>Type</Th><Th>URL</Th><Th>Auth</Th></Tr></Thead>
              <Tbody>
                {sources.map((source) => (
                  <Tr key={`${source.type}-${source.name}`}>
                    <Td dataLabel="Name"><span className="mono">{source.name}</span></Td>
                    <Td dataLabel="Type"><Label color="blue" isCompact>{source.type}</Label></Td>
                    <Td dataLabel="URL"><span className="mono mono--small">{source.url}</span></Td>
                    <Td dataLabel="Auth"><SecretRefView secret={source.auth_header} /></Td>
                  </Tr>
                ))}
              </Tbody>
            </Table>
          )}
        </Panel>
      )}
    </AdminState>
  );
}

export function AdminChannels() {
  const { config, error } = useConfig();
  const shared =
    config !== null &&
    config.notifications.some((notification) =>
      config.approvals.some(
        (approval) =>
          secretRefText(notification.bot_token) === secretRefText(approval.bot_token) &&
          secretRefText(notification.chat_id) === secretRefText(approval.chat_id),
      ),
    );
  const channels =
    config === null
      ? []
      : [
          ...config.notifications.map((channel) => ({ purpose: 'Notifications', channel })),
          ...config.approvals.map((channel) => ({ purpose: 'Approvals', channel })),
        ];
  return (
    <AdminState title="Channels" blurb={blurbs.channels} error={error} loading={config === null}>
      {config !== null && (
        <>
          {shared && (
            <Alert variant="info" isInline title="Approvals and notifications share one destination">
              Anyone who can read the notification channel can also settle an approval. Point approvals at their own chat to
              narrow that audience.
            </Alert>
          )}
          <Panel>
            <PanelHeader>Channels</PanelHeader>
            {channels.length === 0 ? (
              <div className="panel__body">No channels configured.</div>
            ) : (
              <Table variant="compact" aria-label="Channels">
                <Thead><Tr><Th>Purpose</Th><Th>Name</Th><Th>Type</Th><Th>Credentials</Th></Tr></Thead>
                <Tbody>
                  {channels.map(({ purpose, channel }) => (
                    <Tr key={`${purpose}-${channel.name}`}>
                      <Td dataLabel="Purpose">{purpose}</Td>
                      <Td dataLabel="Name"><span className="mono">{channel.name}</span></Td>
                      <Td dataLabel="Type"><Label color="blue" isCompact>{channel.type}</Label></Td>
                      <Td dataLabel="Credentials">
                        <div className="stack stack--tight">
                          <span>Bot token: <SecretRefView secret={channel.bot_token} /></span>
                          <span>Chat ID: <SecretRefView secret={channel.chat_id} /></span>
                        </div>
                      </Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            )}
          </Panel>
        </>
      )}
    </AdminState>
  );
}

function latestSweep(name: string, investigations: Summary[]): Summary | undefined {
  return investigations
    .filter((item) => item.trigger === `sweep:${name}`)
    .sort((a, b) => new Date(b.started_at).getTime() - new Date(a.started_at).getTime())[0];
}

export function AdminSweeps() {
  const [data, setData] = useState<{ config: ConfigView; investigations: Summary[] } | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    Promise.all([api.getConfig(), api.listInvestigations()]).then(
      ([config, investigations]) => setData({ config, investigations }),
      (reason) => setError(String(reason)),
    );
  }, []);
  return (
    <AdminState title="Sweeps" blurb={blurbs.sweeps} error={error} loading={data === null}>
      {data !== null &&
        (data.config.sweeps.length === 0 ? (
          <Panel>
            <EmptyState titleText="No sweeps configured" headingLevel="h2" variant="sm">
              <EmptyStateBody>Scheduled questions will appear here when configured.</EmptyStateBody>
            </EmptyState>
          </Panel>
        ) : (
          <div className="stack">
            {data.config.sweeps.map((sweep) => {
              const latest = latestSweep(sweep.name, data.investigations);
              return (
                <Panel key={sweep.name}>
                  <PanelHeader
                    aside={
                      <span className="inline-cluster">
                        <span className="subtle">last run {latest === undefined ? '—' : relativeTime(latest.started_at)}</span>
                        {latest?.verdict === undefined ? '—' : <VerdictLabel verdict={latest.verdict.verdict} />}
                      </span>
                    }
                  >
                    <span className="inline-cluster">
                      <span className="mono">{sweep.name}</span>
                      <Label variant="outline" isCompact>{sweep.schedule}</Label>
                    </span>
                  </PanelHeader>
                  <div className="panel__body">
                    <ExpandableSection
                      toggleTextCollapsed="Show the question as written"
                      toggleTextExpanded="Hide the question"
                    >
                      <pre className="mono-pre">{sweep.question}</pre>
                    </ExpandableSection>
                  </div>
                </Panel>
              );
            })}
          </div>
        ))}
    </AdminState>
  );
}
