import { useCallback, useEffect, useState } from 'react';
import {
  Alert,
  AlertActionCloseButton,
  Button,
  EmptyState,
  EmptyStateBody,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
} from '@patternfly/react-core';
import { Link } from 'react-router-dom';
import { APIError, api, streamSSE, type ApprovalEvent, type PendingApproval } from '../api';
import { PageHeader, PageLoading } from '../components';
import { relativeTime } from '../utils';

type Decision = { approved: boolean; via: string };

function callText(item: PendingApproval): string {
  let args = item.args_json;
  try {
    args = JSON.stringify(JSON.parse(item.args_json), null, 2);
  } catch {
    // Preserve the daemon's raw arguments when an older record is not valid JSON.
  }
  return `${item.name} ${args}`;
}

export function Approvals() {
  const [items, setItems] = useState<PendingApproval[] | null>(null);
  const [decisions, setDecisions] = useState<Record<string, Decision>>({});
  const [modal, setModal] = useState<{ item: PendingApproval; approved: boolean } | null>(null);
  const [conflict, setConflict] = useState(false);
  const [error, setError] = useState('');
  const [deciding, setDeciding] = useState(false);

  const refresh = useCallback(() => {
    api.listApprovals().then(setItems, (reason) => setError(String(reason)));
  }, []);

  useEffect(() => {
    refresh();
    const abort = new AbortController();
    streamSSE(
      '/v1/approvals/events',
      (_type, data) => {
        const event = JSON.parse(data) as ApprovalEvent;
        setItems((current) => {
          if (current === null) return [event.approval];
          if (event.type === 'requested') {
            return [event.approval, ...current.filter((item) => item.id !== event.approval.id)];
          }
          return current.some((item) => item.id === event.approval.id) ? current : [event.approval, ...current];
        });
        if (event.type === 'settled') {
          setDecisions((current) => ({
            ...current,
            [event.approval.id]: { approved: event.approved, via: event.via },
          }));
        }
      },
      abort.signal,
    ).catch((reason) => {
      if (!abort.signal.aborted) setError(String(reason));
    });
    return () => abort.abort();
  }, [refresh]);

  const decide = () => {
    if (modal === null) return;
    setDeciding(true);
    api.decideApproval(modal.item.id, modal.approved).then(
      () => {
        setDecisions((current) => ({
          ...current,
          [modal.item.id]: { approved: modal.approved, via: 'api' },
        }));
        setDeciding(false);
        setModal(null);
      },
      (reason) => {
        setDeciding(false);
        setModal(null);
        if (reason instanceof APIError && reason.status === 409) {
          setConflict(true);
          refresh();
          return;
        }
        setError(String(reason));
      },
    );
  };

  return (
    <div className="page">
      <PageHeader
        title="Approvals"
        subtitle="Actions that require an operator decision before the investigation can continue."
      />
      {conflict && (
        <Alert
          variant="warning"
          title="Already decided elsewhere — the decision that settled first stands"
          actionClose={<AlertActionCloseButton onClose={() => setConflict(false)} />}
        />
      )}
      {error !== '' && (
        <Alert variant="danger" title="Could not load approvals">
          {error}
        </Alert>
      )}
      {items === null ? (
        error === '' && <PageLoading />
      ) : items.length === 0 ? (
        <div className="panel">
          <EmptyState titleText="Nothing pending" headingLevel="h2" variant="sm">
            <EmptyStateBody>Gated actions will appear here when an investigation needs permission to continue.</EmptyStateBody>
          </EmptyState>
        </div>
      ) : (
        <div className="stack">
          {items.map((item) => {
            const settled = decisions[item.id];
            return (
              <article className="approval-card" key={item.id}>
                <div className="approval-card__header">
                  {settled === undefined ? (
                    <Label color="orangered" isCompact>
                      awaiting
                    </Label>
                  ) : (
                    <Label color={settled.approved ? 'green' : 'red'} isCompact>
                      {settled.approved ? 'approved' : 'denied'} via {settled.via}
                    </Label>
                  )}
                  <span className="mono">{item.name}</span>
                  <span className="subtle" style={{ marginLeft: 'auto' }}>
                    requested {relativeTime(item.requested_at)}
                  </span>
                </div>
                <div className="approval-card__body">
                  <div>
                    <p>{item.human}</p>
                    <p>
                      Investigation{' '}
                      <Link className="mono mono--small" to={`/investigations/${encodeURIComponent(item.investigation_id)}`}>
                        {item.investigation_id}
                      </Link>
                    </p>
                    <pre className="mono-pre">{callText(item)}</pre>
                  </div>
                  {settled === undefined && (
                    <div className="inline-cluster">
                      <Button variant="primary" onClick={() => setModal({ item, approved: true })}>
                        Approve
                      </Button>
                      <Button variant="secondary" isDanger onClick={() => setModal({ item, approved: false })}>
                        Deny
                      </Button>
                    </div>
                  )}
                </div>
              </article>
            );
          })}
        </div>
      )}

      <Modal isOpen={modal !== null} variant="medium" onClose={() => setModal(null)}>
        <ModalHeader
          title={`${modal?.approved === true ? 'Approve' : 'Deny'} this action?`}
          titleIconVariant={modal?.approved === true ? 'warning' : 'danger'}
        />
        <ModalBody>
          {modal !== null && (
            <>
              <p>{modal.item.human}</p>
              <pre className="mono-pre">{callText(modal.item)}</pre>
            </>
          )}
        </ModalBody>
        <ModalFooter>
          <Button
            variant={modal?.approved === true ? 'primary' : 'danger'}
            onClick={decide}
            isLoading={deciding}
            isDisabled={deciding}
          >
            {modal?.approved === true ? 'Approve' : 'Deny'}
          </Button>
          <Button variant="link" onClick={() => setModal(null)} isDisabled={deciding}>
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </div>
  );
}
