import { useCallback, useEffect, useState } from 'react';
import {
  Alert,
  Button,
  Drawer,
  DrawerContent,
  DrawerContentBody,
  DrawerPanelContent,
  EmptyState,
  EmptyStateBody,
  Label,
  SearchInput,
  Tab,
  Tabs,
  TabTitleText,
  ToggleGroup,
  ToggleGroupItem,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { api, streamSSE, type StoredEvent, type Summary } from '../api';
import { PageHeader, PageLoading, Panel, PanelHeader, StatusLabel, VerdictLabel } from '../components';
import { relativeTime } from '../utils';

type EventPayload = Record<string, unknown>;

function EventTypeLabel({ type }: { type: StoredEvent['type'] }) {
  const color =
    type === 'approval_requested'
      ? 'orangered'
      : type === 'approval_decided' || type === 'completed'
        ? 'green'
        : type === 'failed'
          ? 'red'
          : type === 'created'
            ? 'blue'
            : 'grey';
  return (
    <Label color={color} isCompact>
      {type}
    </Label>
  );
}

function EventDetail({ event }: { event: StoredEvent }) {
  const payload = event.payload as EventPayload;
  if (event.type === 'tool_call') {
    return (
      <span className="mono event-detail">
        {String(payload.name ?? '')} {JSON.stringify(payload.args ?? {})}
        {String(payload.result_snippet ?? '') !== '' ? ` → ${String(payload.result_snippet).slice(0, 300)}` : ''}
      </span>
    );
  }
  if (event.type === 'created') return <>{String(payload.question ?? '')}</>;
  if (event.type === 'approval_requested') return <>{String(payload.human ?? '')}</>;
  if (event.type === 'approval_decided') {
    return <>{`${payload.approved === true ? 'approved' : 'denied'} via ${String(payload.via ?? 'unknown')}`}</>;
  }
  if (event.type === 'completed') return <>{String(payload.answer ?? '')}</>;
  return <>{String(payload.reason ?? '')}</>;
}

function EventStream({ events }: { events: StoredEvent[] }) {
  return (
    <div className="drawer-panel">
      <h2 id="event-stream-title">Event stream</h2>
      {events.length === 0 ? (
        <p className="subtle">No events have been recorded yet.</p>
      ) : (
        events.map((event, index) => (
          <div className="event-row" key={`${event.ts}-${event.type}-${index}`}>
            <span className="event-row__time">{new Date(event.ts).toLocaleTimeString()}</span>
            <EventTypeLabel type={event.type} />
            <div className="event-row__detail">
              <EventDetail event={event} />
            </div>
          </div>
        ))
      )}
    </div>
  );
}

function GatedActions({ events }: { events: StoredEvent[] }) {
  const decisions = new Map<string, EventPayload>();
  for (const event of events) {
    if (event.type === 'approval_decided') {
      const payload = event.payload as EventPayload;
      decisions.set(String(payload.call_id ?? ''), payload);
    }
  }
  const requests = events.filter((event) => event.type === 'approval_requested');
  return (
    <Panel>
      <PanelHeader>Gated actions</PanelHeader>
      <div className="panel__body">
        {requests.length === 0 ? (
          <span className="subtle">No gated actions were proposed.</span>
        ) : (
          requests.map((event, index) => {
            const request = event.payload as EventPayload;
            const decision = decisions.get(String(request.call_id ?? ''));
            return (
              <div className="item-row" key={`${String(request.call_id)}-${index}`}>
                <div className="item-row__content stack stack--tight">
                  <span className="mono">{String(request.name ?? '')}</span>
                  <span>{String(request.human ?? '')}</span>
                </div>
                {decision === undefined ? (
                  <Label color="orangered" isCompact>
                    awaiting
                  </Label>
                ) : (
                  <span className="inline-cluster">
                    <Label color={decision.approved === true ? 'green' : 'red'} isCompact>
                      {decision.approved === true ? 'approved' : 'denied'}
                    </Label>
                    <span className="subtle">via {String(decision.via ?? 'unknown')}</span>
                  </span>
                )}
              </div>
            );
          })
        )}
      </div>
    </Panel>
  );
}

export function InvestigationDetail() {
  const { id = '' } = useParams();
  const [summary, setSummary] = useState<Summary | null>(null);
  const [events, setEvents] = useState<StoredEvent[]>([]);
  const [drawerOverride, setDrawerOverride] = useState<boolean | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    const abort = new AbortController();
    setSummary(null);
    setEvents([]);
    setDrawerOverride(null);
    setError('');
    api.getInvestigation(id).then(
      (investigation) => {
        setSummary(investigation.summary);
        if (investigation.summary.status !== 'running') {
          setEvents(investigation.events);
          return;
        }
        streamSSE(
          `/v1/investigations/${encodeURIComponent(id)}/events`,
          (_type, data) => {
            const event = JSON.parse(data) as StoredEvent;
            setEvents((current) => [...current, event]);
            if (event.type === 'completed' || event.type === 'failed') {
              api.getInvestigation(id).then((finished) => setSummary(finished.summary), () => undefined);
            }
          },
          abort.signal,
        ).catch((reason) => {
          if (!abort.signal.aborted) setError(String(reason));
        });
      },
      (reason) => setError(String(reason)),
    );
    return () => abort.abort();
  }, [id]);

  const drawerOpen = drawerOverride ?? summary?.status === 'running';
  const failed = events.find((event) => event.type === 'failed');
  const failedReason = failed === undefined ? '' : String((failed.payload as EventPayload).reason ?? '');

  const panelContent = (
    <DrawerPanelContent defaultSize="460px" aria-labelledby="event-stream-title">
      <EventStream events={events} />
    </DrawerPanelContent>
  );

  return (
    <Drawer isExpanded={drawerOpen} isInline position="end">
      <DrawerContent panelContent={panelContent}>
        <DrawerContentBody>
          <div className="page">
            {error !== '' && (
              <Alert variant="danger" title="Could not load the investigation">
                {error}
              </Alert>
            )}
            {summary === null ? (
              error === '' && <PageLoading />
            ) : (
              <>
                <PageHeader
                  title={summary.question}
                  subtitle="Investigation record"
                  eyebrow={<Link to="/investigations">← All investigations</Link>}
                />
                <div className="detail-meta">
                  <StatusLabel status={summary.status} />
                  <Label variant="outline" isCompact>
                    {summary.trigger}
                  </Label>
                  <span>started {new Date(summary.started_at).toLocaleString()}</span>
                  <span className="mono mono--small">{summary.id}</span>
                  <Button variant="link" isInline onClick={() => setDrawerOverride(!drawerOpen)}>
                    {drawerOpen ? 'Hide event stream' : 'Show event stream'}
                  </Button>
                </div>

                {summary.verdict === undefined ? (
                  <Panel>
                    <PanelHeader>Verdict</PanelHeader>
                    <div className="panel__body">
                      {summary.status === 'running'
                        ? 'This investigation is still running; a verdict has not been written yet.'
                        : summary.status === 'failed'
                          ? `The investigation failed before producing a verdict${failedReason === '' ? '.' : `: ${failedReason}`}`
                          : 'This investigation completed before structured verdicts were recorded.'}
                    </div>
                  </Panel>
                ) : (
                  <Panel>
                    <PanelHeader
                      aside={
                        <span className="inline-cluster">
                          <VerdictLabel verdict={summary.verdict.verdict} />
                          {summary.verdict.resolution !== undefined && (
                            <Label
                              color={summary.verdict.resolution === 'cleared' ? 'green' : 'red'}
                              variant="outline"
                              isCompact
                            >
                              {summary.verdict.resolution}
                            </Label>
                          )}
                        </span>
                      }
                    >
                      Verdict
                    </PanelHeader>
                    <div className="panel__body">
                      <p>{summary.verdict.summary}</p>
                      {summary.verdict.evidence.length > 0 && (
                        <div className="panel__section-title">Evidence</div>
                      )}
                      {summary.verdict.evidence.length > 0 && (
                        <ul className="evidence-list mono mono--small">
                          {summary.verdict.evidence.map((evidence, index) => (
                            <li key={index}>{evidence}</li>
                          ))}
                        </ul>
                      )}
                    </div>
                  </Panel>
                )}

                <Panel>
                  <PanelHeader>Answer</PanelHeader>
                  <div className="panel__body answer-text">
                    {summary.answer === undefined || summary.answer === '' ? (
                      <span className="subtle">No answer was recorded.</span>
                    ) : (
                      summary.answer
                    )}
                  </div>
                </Panel>
                <GatedActions events={events} />
              </>
            )}
          </div>
        </DrawerContentBody>
      </DrawerContent>
    </Drawer>
  );
}

export function Investigations() {
  const navigate = useNavigate();
  const [items, setItems] = useState<Summary[] | null>(null);
  const [tab, setTab] = useState<'questions' | 'sweeps'>('questions');
  const [query, setQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState<string[]>([]);
  const [verdictFilter, setVerdictFilter] = useState<string[]>([]);
  const [error, setError] = useState('');

  const refresh = useCallback(() => {
    api.listInvestigations().then(setItems, (reason) => setError(String(reason)));
  }, []);

  useEffect(() => {
    refresh();
    const timer = window.setInterval(refresh, 5000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  const questions = items?.filter((item) => !item.trigger.startsWith('sweep')) ?? [];
  const sweeps = items?.filter((item) => item.trigger.startsWith('sweep')) ?? [];
  const tabItems = tab === 'questions' ? questions : sweeps;
  const normalizedQuery = query.trim().toLowerCase();
  const visible = tabItems.filter((item) => {
    if (normalizedQuery !== '' && !item.question.toLowerCase().includes(normalizedQuery)) return false;
    if (statusFilter.length > 0 && !statusFilter.includes(item.status)) return false;
    const verdict = item.verdict?.verdict ?? 'none';
    return verdictFilter.length === 0 || verdictFilter.includes(verdict);
  });
  const filtersActive = query !== '' || statusFilter.length > 0 || verdictFilter.length > 0;

  const toggleStatus = (status: string, selected: boolean) => {
    setStatusFilter((current) => selected ? [...current, status] : current.filter((item) => item !== status));
  };
  const toggleVerdict = (verdict: string, selected: boolean) => {
    setVerdictFilter((current) => selected ? [...current, verdict] : current.filter((item) => item !== verdict));
  };
  const clearFilters = () => {
    setQuery('');
    setStatusFilter([]);
    setVerdictFilter([]);
  };

  return (
    <div className="page page--flush page--tabs">
      <PageHeader
        title="Investigations"
        subtitle="Questions, alerts, and scheduled sweeps recorded by the daemon."
      />
      {error !== '' && (
        <Alert variant="danger" title="Could not load investigations">
          {error}
        </Alert>
      )}
      <div className="tab-strip">
        <Tabs
          activeKey={tab}
          onSelect={(_event, key) => setTab(String(key) === 'sweeps' ? 'sweeps' : 'questions')}
          aria-label="Investigation categories"
        >
          <Tab eventKey="questions" title={<TabTitleText>Questions ({questions.length})</TabTitleText>} />
          <Tab eventKey="sweeps" title={<TabTitleText>Sweeps ({sweeps.length})</TabTitleText>} />
        </Tabs>
      </div>
      <div className="filter-toolbar">
        <SearchInput
          className="filter-toolbar__search"
          value={query}
          placeholder="Filter by question"
          aria-label="Filter investigations by question"
          onChange={(_event, value) => setQuery(value)}
          onClear={() => setQuery('')}
        />
        <ToggleGroup isCompact aria-label="Filter investigations by status">
          {['running', 'completed', 'failed'].map((status) => (
            <ToggleGroupItem
              key={status}
              text={`${status[0].toUpperCase()}${status.slice(1)}`}
              buttonId={`investigation-status-${status}`}
              isSelected={statusFilter.includes(status)}
              onChange={(_event, selected) => toggleStatus(status, selected)}
            />
          ))}
        </ToggleGroup>
        <ToggleGroup isCompact aria-label="Filter investigations by verdict">
          {[
            ['healthy', 'Healthy'],
            ['attention', 'Attention'],
            ['action', 'Action'],
            ['none', 'No verdict'],
          ].map(([verdict, label]) => (
            <ToggleGroupItem
              key={verdict}
              text={label}
              buttonId={`investigation-verdict-${verdict}`}
              isSelected={verdictFilter.includes(verdict)}
              onChange={(_event, selected) => toggleVerdict(verdict, selected)}
            />
          ))}
        </ToggleGroup>
        <div className="filter-toolbar__summary">
          <span className="subtle">
            {filtersActive
              ? `${visible.length} of ${tabItems.length}`
              : `${tabItems.length} ${tabItems.length === 1 ? 'item' : 'items'}`}
          </span>
          {filtersActive && (
            <Button variant="link" isInline onClick={clearFilters}>Clear all filters</Button>
          )}
        </div>
      </div>
      <section className="flush-table">
        {items === null ? (
          error === '' && <PageLoading />
        ) : visible.length === 0 && filtersActive ? (
          <EmptyState titleText="No investigations match these filters" headingLevel="h3" variant="sm">
            <EmptyStateBody>Widen the state filters or clear the search.</EmptyStateBody>
          </EmptyState>
        ) : tabItems.length === 0 ? (
          <EmptyState titleText={tab === 'questions' ? 'No questions yet' : 'No sweeps yet'} headingLevel="h2" variant="sm">
            <EmptyStateBody>
              {tab === 'questions'
                ? 'New questions and alerts will appear here.'
                : 'Scheduled investigations will appear here after they run.'}
            </EmptyStateBody>
          </EmptyState>
        ) : (
          <Table variant="compact" aria-label={`${tab} investigations`}>
            <Thead>
              <Tr>
                <Th>{tab === 'sweeps' ? 'Sweep' : 'Question'}</Th>
                <Th>Trigger</Th>
                <Th>Status</Th>
                <Th>Verdict</Th>
                <Th>Started</Th>
              </Tr>
            </Thead>
            <Tbody>
              {visible.map((item) => (
                <Tr
                  key={item.id}
                  isClickable
                  onRowClick={() => navigate(`/investigations/${encodeURIComponent(item.id)}`)}
                >
                  <Td dataLabel="Question">
                    <Link className="table-link" to={`/investigations/${encodeURIComponent(item.id)}`}>
                      {item.question}
                    </Link>
                  </Td>
                  <Td dataLabel="Trigger">
                    <Label variant="outline" isCompact>
                      {item.trigger}
                    </Label>
                  </Td>
                  <Td dataLabel="Status">
                    <StatusLabel status={item.status} />
                  </Td>
                  <Td dataLabel="Verdict">
                    {item.verdict === undefined ? '—' : <VerdictLabel verdict={item.verdict.verdict} />}
                  </Td>
                  <Td dataLabel="Started" title={new Date(item.started_at).toLocaleString()}>
                    {relativeTime(item.started_at)}
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </section>
    </div>
  );
}
