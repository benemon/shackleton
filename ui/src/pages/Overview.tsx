import { useEffect, useState } from 'react';
import {
  Alert,
  Badge,
  Button,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  Label,
  TextInput,
} from '@patternfly/react-core';
import { CheckCircleIcon } from '@patternfly/react-icons';
import { Link, useNavigate } from 'react-router-dom';
import {
  api,
  type ConfigView,
  type Health,
  type Inventory,
  type KBArticleMeta,
  type PendingApproval,
  type Summary,
} from '../api';
import { PageHeader, PageLoading, Panel, PanelHeader } from '../components';
import { endpointHost, relativeTime } from '../utils';

type OverviewData = {
  investigations: Summary[];
  approvals: PendingApproval[];
  inventory: Inventory;
  articles: KBArticleMeta[];
  config: ConfigView;
  health: Health;
};

export function Overview() {
  const navigate = useNavigate();
  const [data, setData] = useState<OverviewData | null>(null);
  const [question, setQuestion] = useState('');
  const [asking, setAsking] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    Promise.all([
      api.listInvestigations(),
      api.listApprovals(),
      api.getInventory(),
      api.listKB(),
      api.getConfig(),
      api.getHealth(),
    ]).then(
      ([investigations, approvals, inventory, articles, config, health]) =>
        setData({ investigations, approvals, inventory, articles, config, health }),
      (reason) => setError(String(reason)),
    );
  }, []);

  const ask = () => {
    const value = question.trim();
    if (value === '') return;
    setAsking(true);
    api.createInvestigation(value).then(
      (created) => navigate(`/investigations/${encodeURIComponent(created.id)}`),
      (reason) => {
        setAsking(false);
        setError(String(reason));
      },
    );
  };

  if (error !== '') {
    return (
      <div className="page">
        <PageHeader title="Overview" subtitle="Live daemon and estate state." />
        <Alert variant="danger" title="Could not load the overview">
          {error}
        </Alert>
      </div>
    );
  }
  if (data === null) {
    return (
      <div className="page">
        <PageHeader title="Overview" subtitle="Loading current daemon and estate state." />
        <PageLoading />
      </div>
    );
  }

  const running = data.investigations.filter((item) => item.status === 'running');
  const drafts = data.inventory.hosts.filter((host) => host.status === 'draft');
  const ignored = data.inventory.hosts.filter((host) => host.status === 'ignored');
  const declared = data.inventory.hosts.filter(
    (host) => host.status === 'approved' || (host.status === undefined && host.cluster === undefined),
  );
  const clusterMembers = data.inventory.hosts.filter((host) => host.cluster !== undefined);
  const draftArticles = data.articles.filter((article) => article.status === 'draft');
  const approvedArticles = data.articles.filter((article) => article.status === 'approved');

  return (
    <div className="page">
      <PageHeader
        title="Overview"
        subtitle={`${running.length} running · ${data.approvals.length} awaiting approval · ${declared.length} declared hosts · ${data.inventory.clusters.length} clusters`}
      />

      <Panel>
        <div className="panel__body">
          <label htmlFor="overview-question" style={{ fontWeight: 500 }}>Ask the estate a question</label>
          <Form
            className="ask-form"
            onSubmit={(event) => {
              event.preventDefault();
              ask();
            }}
          >
            <TextInput
              id="overview-question"
              aria-label="Ask an infrastructure question"
              placeholder="Ask a question about the estate…"
              value={question}
              onChange={(_event, value) => setQuestion(value)}
            />
            <Button type="submit" variant="primary" isLoading={asking} isDisabled={asking || question.trim() === ''}>
              Investigate
            </Button>
          </Form>
          <div className="subtle">Answers stream live. Mutating actions stop for your approval.</div>
        </div>
      </Panel>

      <div className="panel-grid">
        <Panel>
          <PanelHeader aside={<Badge>{data.approvals.length}</Badge>}>Pending approvals</PanelHeader>
          <div className="panel__body">
            {data.approvals.length === 0 ? (
              <span className="subtle">No actions are awaiting a decision.</span>
            ) : (
              data.approvals.slice(0, 2).map((approval) => (
                <div className="item-row" key={approval.id}>
                  <div className="item-row__content stack stack--tight">
                    <span className="mono mono--small">{approval.name}</span>
                    <span>{approval.human}</span>
                    <span className="subtle">requested {relativeTime(approval.requested_at)}</span>
                  </div>
                </div>
              ))
            )}
            <div className="panel-footnote">
              <Link to="/approvals">Review approvals</Link>
            </div>
          </div>
        </Panel>

        <Panel>
          <PanelHeader aside={<Badge>{running.length}</Badge>}>Running now</PanelHeader>
          <div className="panel__body">
            {running.length === 0 ? (
              <span className="subtle">No investigations are running.</span>
            ) : (
              running.map((item) => (
                <div className="item-row" key={item.id}>
                  <div className="item-row__content stack stack--tight">
                    <Link className="table-link" to={`/investigations/${encodeURIComponent(item.id)}`}>
                      {item.question}
                    </Link>
                    <span className="subtle">started {relativeTime(item.started_at)}</span>
                  </div>
                </div>
              ))
            )}
            <div className="panel-footnote">
              <Link to="/investigations">All investigations</Link>
            </div>
          </div>
        </Panel>

        <Panel>
          <PanelHeader>Estate</PanelHeader>
          <div className="panel__body">
            <DescriptionList isHorizontal isCompact>
              <DescriptionListGroup>
                <DescriptionListTerm>Declared hosts</DescriptionListTerm>
                <DescriptionListDescription>
                  {declared.length}
                  {declared.length > 0 ? ` — ${declared.map((host) => host.name).join(', ')}` : ''}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Clusters</DescriptionListTerm>
                <DescriptionListDescription>
                  {data.inventory.clusters.length}
                  {data.inventory.clusters.length > 0
                    ? ` — ${data.inventory.clusters
                        .map(
                          (cluster) =>
                            `${cluster.name} (${clusterMembers.filter((host) => host.cluster === cluster.name).length} members)`,
                        )
                        .join(', ')}`
                    : ''}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Drafts</DescriptionListTerm>
                <DescriptionListDescription>
                  <span className="inline-cluster">
                    {drafts.length} awaiting approval
                    <Label color="grey" isCompact>
                      inert
                    </Label>
                  </span>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Ignored</DescriptionListTerm>
                <DescriptionListDescription>{ignored.length} — dismissed by an operator</DescriptionListDescription>
              </DescriptionListGroup>
            </DescriptionList>
          </div>
        </Panel>

        <Panel>
          <PanelHeader>Knowledge base</PanelHeader>
          <div className="panel__body">
            {draftArticles.length === 0 ? (
              <span className="subtle">No draft articles.</span>
            ) : (
              draftArticles.map((article) => (
                <div className="item-row" key={article.slug}>
                  <div className="item-row__content">
                    <Link className="table-link" to={`/kb/${encodeURIComponent(article.slug)}`}>
                      {article.title}
                    </Link>
                  </div>
                  <Label color={article.nominated === true ? 'purple' : 'grey'} isCompact>
                    {article.nominated === true ? 'nominated' : 'draft'}
                  </Label>
                </div>
              ))
            )}
            <div className="panel-footnote">
              {approvedArticles.length} approved {approvedArticles.length === 1 ? 'article guides' : 'articles guide'} live
              investigations
            </div>
          </div>
        </Panel>

        <Panel className="panel--wide">
          <PanelHeader>Daemon</PanelHeader>
          <div className="panel__body">
            <DescriptionList isHorizontal isCompact columnModifier={{ lg: '3Col' }}>
              <DescriptionListGroup>
                <DescriptionListTerm>Status</DescriptionListTerm>
                <DescriptionListDescription>
                  <Label color="green" isCompact icon={<CheckCircleIcon />}>
                    {data.health.status}
                  </Label>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Model</DescriptionListTerm>
                <DescriptionListDescription>
                  <span className="mono">{data.config.model.name}</span>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Endpoint</DescriptionListTerm>
                <DescriptionListDescription>
                  <span className="mono mono--small">{endpointHost(data.config.model.base_url)}</span>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Gated tools</DescriptionListTerm>
                <DescriptionListDescription>
                  <span className="mono mono--small">{data.config.gated_tools.join(', ') || '—'}</span>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Approval channels</DescriptionListTerm>
                <DescriptionListDescription>
                  {data.config.approvals.map((channel) => channel.name).join(', ') || '—'}
                </DescriptionListDescription>
              </DescriptionListGroup>
            </DescriptionList>
          </div>
        </Panel>
      </div>
    </div>
  );
}
