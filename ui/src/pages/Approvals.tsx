import { useCallback, useEffect, useState } from 'react';
import { Alert, Button, EmptyState, PageSection, Title } from '@patternfly/react-core';
import { api, streamSSE, type PendingApproval } from '../api';

export function Approvals() {
  const [items, setItems] = useState<PendingApproval[]>([]);
  const [error, setError] = useState('');

  const refresh = useCallback(() => {
    api.listApprovals().then(setItems, (e) => setError(String(e)));
  }, []);

  useEffect(() => {
    refresh();
    const abort = new AbortController();
    streamSSE('/v1/approvals/events', () => refresh(), abort.signal).catch((e) => {
      if (!abort.signal.aborted) setError(String(e));
    });
    return () => abort.abort();
  }, [refresh]);

  const decide = (id: string, approved: boolean) => {
    api.decideApproval(id, approved).then(refresh, (e) => setError(String(e)));
  };

  return (
    <PageSection>
      <Title headingLevel="h2">Pending approvals</Title>
      {error !== '' && <Alert variant="danger" title={error} />}
      {items.length === 0 ? (
        <EmptyState titleText="Nothing pending" headingLevel="h3" />
      ) : (
        <table className="pf-v6-c-table pf-m-compact pf-m-grid-md" role="grid">
          <thead className="pf-v6-c-table__thead">
            <tr className="pf-v6-c-table__tr">
              <th className="pf-v6-c-table__th">Proposed action</th>
              <th className="pf-v6-c-table__th">Requested</th>
              <th className="pf-v6-c-table__th">Decision</th>
            </tr>
          </thead>
          <tbody className="pf-v6-c-table__tbody">
            {items.map((item) => (
              <tr key={item.id} className="pf-v6-c-table__tr">
                <td className="pf-v6-c-table__td" style={{ whiteSpace: 'pre-wrap' }}>
                  {item.human}
                </td>
                <td className="pf-v6-c-table__td">{new Date(item.requested_at).toLocaleTimeString()}</td>
                <td className="pf-v6-c-table__td" style={{ whiteSpace: 'nowrap' }}>
                  <Button variant="primary" size="sm" onClick={() => decide(item.id, true)}>
                    Approve
                  </Button>{' '}
                  <Button variant="danger" size="sm" onClick={() => decide(item.id, false)}>
                    Deny
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </PageSection>
  );
}
