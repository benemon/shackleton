import { useEffect, useState } from 'react';
import {
  Alert,
  Button,
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
import { BanIcon, CheckCircleIcon, ExclamationTriangleIcon } from '@patternfly/react-icons';
import { ExpandableRowContent, Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { api, type Inventory as InventoryView } from '../api';
import { PageHeader, PageLoading } from '../components';

type Host = InventoryView['hosts'][number];

function valueOrDash(value: string | undefined): string {
  return value === undefined || value === '' ? '—' : value;
}

function shortDate(value: string | undefined): string {
  if (value === undefined) return '—';
  return new Date(value).toLocaleDateString(undefined, { day: '2-digit', month: 'short', year: 'numeric' });
}

export function Inventory() {
  const [inventory, setInventory] = useState<InventoryView | null>(null);
  const [expanded, setExpanded] = useState<Record<number, boolean>>({});
  const [error, setError] = useState('');

  useEffect(() => {
    api.getInventory().then(setInventory, (reason) => setError(String(reason)));
  }, []);

  return (
    <div className="page page--flush page--tabs">
      <PageHeader
        title="Inventory"
        subtitle="Only declared hosts can be targeted by a gated action. Every other state is a recognized fact the agent may read but never act on."
      />
      {error !== '' && (
        <Alert variant="danger" title="Could not load inventory">
          {error}
        </Alert>
      )}
      {inventory === null ? (
        error === '' && <PageLoading />
      ) : (
        <InventoryPanels inventory={inventory} expanded={expanded} setExpanded={setExpanded} />
      )}
    </div>
  );
}

function InventoryPanels({
  inventory,
  expanded,
  setExpanded,
}: {
  inventory: InventoryView;
  expanded: Record<number, boolean>;
  setExpanded: React.Dispatch<React.SetStateAction<Record<number, boolean>>>;
}) {
  const [tab, setTab] = useState<'hosts' | 'clusters'>('hosts');
  const [query, setQuery] = useState('');
  const [stateFilter, setStateFilter] = useState<string[]>([]);
  const hosts = inventory.hosts.filter((host) => host.cluster === undefined);
  const actionable = hosts.filter((host) => host.status === undefined || host.status === 'approved').length;
  const normalizedQuery = query.trim().toLowerCase();
  const visibleHosts = hosts.filter((host) => {
    if (normalizedQuery !== '' && !`${host.name} ${host.hostname ?? ''}`.toLowerCase().includes(normalizedQuery)) {
      return false;
    }
    const state = host.status === undefined || host.status === 'approved' ? 'declared' : host.status;
    return stateFilter.length === 0 || stateFilter.includes(state);
  });
  const filtersActive = query !== '' || stateFilter.length > 0;

  const toggleState = (state: string, selected: boolean) => {
    setStateFilter((current) => selected ? [...current, state] : current.filter((item) => item !== state));
  };
  const clearFilters = () => {
    setQuery('');
    setStateFilter([]);
  };

  return (
    <>
      <div className="tab-strip">
        <Tabs
          activeKey={tab}
          onSelect={(_event, key) => setTab(String(key) === 'clusters' ? 'clusters' : 'hosts')}
          aria-label="Inventory categories"
        >
          <Tab eventKey="hosts" title={<TabTitleText>Hosts ({visibleHosts.length})</TabTitleText>} />
          <Tab eventKey="clusters" title={<TabTitleText>Clusters ({inventory.clusters.length})</TabTitleText>} />
        </Tabs>
      </div>

      {tab === 'hosts' ? (
        <>
          <div className="filter-toolbar">
            <SearchInput
              className="filter-toolbar__search"
              value={query}
              placeholder="Filter by name or hostname"
              aria-label="Filter hosts by name or hostname"
              onChange={(_event, value) => setQuery(value)}
              onClear={() => setQuery('')}
            />
            <ToggleGroup isCompact aria-label="Filter hosts by state">
              {['declared', 'draft', 'ignored'].map((state) => (
                <ToggleGroupItem
                  key={state}
                  text={`${state[0].toUpperCase()}${state.slice(1)}`}
                  buttonId={`host-state-${state}`}
                  isSelected={stateFilter.includes(state)}
                  onChange={(_event, selected) => toggleState(state, selected)}
                />
              ))}
            </ToggleGroup>
            <div className="filter-toolbar__summary">
              <span className="subtle">
                {filtersActive
                  ? `${visibleHosts.length} of ${hosts.length}`
                  : `${hosts.length} hosts · ${actionable} actionable`}
              </span>
              {filtersActive && (
                <Button variant="link" isInline onClick={clearFilters}>Clear all filters</Button>
              )}
            </div>
          </div>
          <section className="flush-table">
            {visibleHosts.length === 0 ? (
              <EmptyState titleText="No hosts match these filters" headingLevel="h3" variant="sm">
                <EmptyStateBody>Widen the state filters or clear the search.</EmptyStateBody>
              </EmptyState>
            ) : (
              <Table variant="compact" aria-label="Hosts">
                <Thead>
                  <Tr>
                    <Th>Name</Th>
                    <Th>Hostname</Th>
                    <Th>Connection</Th>
                    <Th>Known via</Th>
                    <Th>State</Th>
                  </Tr>
                </Thead>
                <Tbody>
                  {visibleHosts.map((host) => {
                    const inert = host.status === 'draft' || host.status === 'ignored';
                    return (
                      <Tr className={inert ? 'inert-row' : undefined} key={host.name}>
                        <Td dataLabel="Name"><span className="mono">{host.name}</span></Td>
                        <Td dataLabel="Hostname">{valueOrDash(host.hostname)}</Td>
                        <Td dataLabel="Connection">{host.connection}</Td>
                        <Td dataLabel="Known via">
                          {inert ? `${valueOrDash(host.source)} · ${shortDate(host.first_seen)}` : 'declared'}
                        </Td>
                        <Td dataLabel="State">
                          {host.status === 'draft' ? (
                            <Label color="grey" isCompact icon={<ExclamationTriangleIcon />}>draft · inert</Label>
                          ) : host.status === 'ignored' ? (
                            <Label color="grey" isCompact icon={<BanIcon />}>ignored · inert</Label>
                          ) : (
                            <Label color="green" isCompact icon={<CheckCircleIcon />}>
                              {host.status === 'approved' ? 'approved' : 'declared'}
                            </Label>
                          )}
                        </Td>
                      </Tr>
                    );
                  })}
                </Tbody>
              </Table>
            )}
          </section>
        </>
      ) : (
        <section className="flush-table">
          <div className="section-header">
            <span className="subtle">Expand a cluster to see its members</span>
          </div>
          {inventory.clusters.length === 0 ? (
            <EmptyState titleText="No clusters" headingLevel="h2" variant="sm">
              <EmptyStateBody>Declared cluster inventory will appear here.</EmptyStateBody>
            </EmptyState>
          ) : (
            <Table variant="compact" aria-label="Clusters">
              <Thead>
                <Tr>
                  <Th aria-label="Expand" />
                  <Th>Name</Th>
                  <Th>Type</Th>
                  <Th>API</Th>
                  <Th>Members</Th>
                </Tr>
              </Thead>
              {inventory.clusters.map((cluster, rowIndex) => {
                const members = inventory.hosts.filter((host) => host.cluster === cluster.name);
                const isExpanded = expanded[rowIndex] === true;
                return (
                  <Tbody key={cluster.name} isExpanded={isExpanded}>
                    <Tr>
                      <Td
                        expand={{
                          rowIndex,
                          isExpanded,
                          onToggle: (_event, index, open) =>
                            setExpanded((current) => ({ ...current, [index]: open })),
                        }}
                      />
                      <Td dataLabel="Name"><span className="mono">{cluster.name}</span></Td>
                      <Td dataLabel="Type"><Label color="blue" isCompact>{cluster.type}</Label></Td>
                      <Td dataLabel="API"><span className="mono mono--small">{cluster.api}</span></Td>
                      <Td dataLabel="Members">{members.length}</Td>
                    </Tr>
                    <Tr isExpanded={isExpanded}>
                      <Td colSpan={5}>
                        <ExpandableRowContent>
                          {members.length === 0 ? (
                            <span className="subtle">No discovered members.</span>
                          ) : (
                            <MemberTable members={members} />
                          )}
                        </ExpandableRowContent>
                      </Td>
                    </Tr>
                  </Tbody>
                );
              })}
            </Table>
          )}
        </section>
      )}
    </>
  );
}

function MemberTable({ members }: { members: Host[] }) {
  return (
    <Table variant="compact" aria-label="Cluster members">
      <Thead>
        <Tr>
          <Th>Member</Th>
          <Th>Seen via</Th>
          <Th>First seen</Th>
          <Th>State</Th>
        </Tr>
      </Thead>
      <Tbody>
        {members.map((member) => (
          <Tr key={member.name}>
            <Td dataLabel="Member"><span className="mono">{member.name}</span></Td>
            <Td dataLabel="Seen via">{valueOrDash(member.source)}</Td>
            <Td dataLabel="First seen">
              {member.first_seen === undefined ? '—' : new Date(member.first_seen).toLocaleString()}
            </Td>
            <Td dataLabel="State">
              <Label color="grey" isCompact>cluster member · inert</Label>
            </Td>
          </Tr>
        ))}
      </Tbody>
    </Table>
  );
}
