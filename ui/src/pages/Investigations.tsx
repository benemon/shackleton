import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Form,
  Label,
  PageSection,
  Spinner,
  TextInput,
  Title,
} from '@patternfly/react-core';
import { api, streamSSE, type StoredEvent, type Summary } from '../api';

function StatusLabel({ status }: { status: string }) {
  const color = status === 'completed' ? 'green' : status === 'failed' ? 'red' : 'blue';
  return <Label color={color}>{status}</Label>;
}

function EventRow({ event }: { event: StoredEvent }) {
  const payload = event.payload as Record<string, unknown>;
  let detail = '';
  switch (event.type) {
    case 'created':
      detail = String(payload.question ?? '');
      break;
    case 'tool_call':
      detail = `${payload.name} ${JSON.stringify(payload.args ?? {})} → ${String(payload.result_snippet ?? '').slice(0, 200)}`;
      break;
    case 'approval_requested':
      detail = String(payload.human ?? '');
      break;
    case 'approval_decided':
      detail = `${payload.approved ? 'approved' : 'denied'} via ${payload.via}`;
      break;
    case 'completed':
      detail = String(payload.answer ?? '');
      break;
    case 'failed':
      detail = String(payload.reason ?? '');
      break;
  }
  return (
    <tr className="pf-v6-c-table__tr">
      <td className="pf-v6-c-table__td" style={{ whiteSpace: 'nowrap' }}>
        {new Date(event.ts).toLocaleTimeString()}
      </td>
      <td className="pf-v6-c-table__td">{event.type}</td>
      <td className="pf-v6-c-table__td" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
        {detail}
      </td>
    </tr>
  );
}

function Detail({ id, onBack }: { id: string; onBack: () => void }) {
  const [summary, setSummary] = useState<Summary | null>(null);
  const [events, setEvents] = useState<StoredEvent[]>([]);
  const [error, setError] = useState('');

  useEffect(() => {
    const abort = new AbortController();
    setEvents([]);
    api
      .getInvestigation(id)
      .then((inv) => {
        setSummary(inv.summary);
        if (inv.summary.status !== 'running') {
          setEvents(inv.events);
          return;
        }
        // Running: replace the snapshot with the SSE stream (replay + live).
        streamSSE(
          `/v1/investigations/${encodeURIComponent(id)}/events`,
          (_type, data) => {
            const event = JSON.parse(data) as StoredEvent;
            setEvents((prev) => [...prev, event]);
            if (event.type === 'completed' || event.type === 'failed') {
              api.getInvestigation(id).then((done) => setSummary(done.summary), () => undefined);
            }
          },
          abort.signal,
        ).catch((e) => {
          if (!abort.signal.aborted) setError(String(e));
        });
      })
      .catch((e) => setError(String(e)));
    return () => abort.abort();
  }, [id]);

  return (
    <PageSection>
      <Button variant="link" onClick={onBack} style={{ paddingLeft: 0 }}>
        ← All investigations
      </Button>
      {error !== '' && <Alert variant="danger" title={error} />}
      {summary === null ? (
        <Spinner />
      ) : (
        <>
          <Title headingLevel="h2">{summary.question}</Title>
          <p>
            <StatusLabel status={summary.status} /> <Label variant="outline">{summary.trigger}</Label>{' '}
            {new Date(summary.started_at).toLocaleString()}
          </p>
          {summary.answer !== undefined && summary.answer !== '' && (
            <Card style={{ margin: '1rem 0' }}>
              <CardTitle>Answer</CardTitle>
              <CardBody style={{ whiteSpace: 'pre-wrap' }}>{summary.answer}</CardBody>
            </Card>
          )}
          <table className="pf-v6-c-table pf-m-compact pf-m-grid-md" role="grid">
            <thead className="pf-v6-c-table__thead">
              <tr className="pf-v6-c-table__tr">
                <th className="pf-v6-c-table__th">Time</th>
                <th className="pf-v6-c-table__th">Event</th>
                <th className="pf-v6-c-table__th">Detail</th>
              </tr>
            </thead>
            <tbody className="pf-v6-c-table__tbody">
              {events.map((event, index) => (
                <EventRow key={index} event={event} />
              ))}
            </tbody>
          </table>
        </>
      )}
    </PageSection>
  );
}

export function Investigations() {
  const [items, setItems] = useState<Summary[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [question, setQuestion] = useState('');
  const [error, setError] = useState('');
  const timer = useRef<number>(0);

  const refresh = useCallback(() => {
    api.listInvestigations().then(setItems, (e) => setError(String(e)));
  }, []);

  const submit = () => {
    if (question.trim() === '') return;
    api.createInvestigation(question.trim()).then(
      (created) => {
        setQuestion('');
        setSelected(created.id);
      },
      (err) => setError(String(err)),
    );
  };

  useEffect(() => {
    if (selected !== null) return;
    refresh();
    timer.current = window.setInterval(refresh, 5000);
    return () => window.clearInterval(timer.current);
  }, [refresh, selected]);

  if (selected !== null) return <Detail id={selected} onBack={() => setSelected(null)} />;

  return (
    <PageSection>
      <Title headingLevel="h2">Investigations</Title>
      {error !== '' && <Alert variant="danger" title={error} />}
      <Form
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
        style={{ margin: '1rem 0', display: 'flex', gap: '0.5rem' }}
      >
        <TextInput
          id="question"
          placeholder="Ask a question…"
          value={question}
          onChange={(_e, v) => setQuestion(v)}
          aria-label="Question"
        />
        <Button type="button" onClick={submit}>
          Investigate
        </Button>
      </Form>
      <table className="pf-v6-c-table pf-m-compact pf-m-grid-md" role="grid">
        <thead className="pf-v6-c-table__thead">
          <tr className="pf-v6-c-table__tr">
            <th className="pf-v6-c-table__th">Question</th>
            <th className="pf-v6-c-table__th">Trigger</th>
            <th className="pf-v6-c-table__th">Status</th>
            <th className="pf-v6-c-table__th">Started</th>
          </tr>
        </thead>
        <tbody className="pf-v6-c-table__tbody">
          {items.map((item) => (
            <tr key={item.id} className="pf-v6-c-table__tr" style={{ cursor: 'pointer' }} onClick={() => setSelected(item.id)}>
              <td className="pf-v6-c-table__td">{item.question.slice(0, 120)}</td>
              <td className="pf-v6-c-table__td">{item.trigger}</td>
              <td className="pf-v6-c-table__td">
                <StatusLabel status={item.status} />
              </td>
              <td className="pf-v6-c-table__td">{new Date(item.started_at).toLocaleString()}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </PageSection>
  );
}
