import { useEffect, useState } from 'react';
import { Alert, EmptyState, EmptyStateBody, Label } from '@patternfly/react-core';
import { ExpandableRowContent, Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { api, type Inventory as InventoryView } from '../api';
import { PageHeader, PageLoading, Panel, PanelHeader } from '../components';

type Host = InventoryView['hosts'][number];

function valueOrDash(value: string | undefined): string {
  return value === undefined || value === '' ? '—' : value;
}

export function Inventory() {
  const [inventory, setInventory] = useState<InventoryView | null>(null);
  const [expanded, setExpanded] = useState<Record<number, boolean>>({});
  const [error, setError] = useState('');

  useEffect(() => {
    api.getInventory().then(setInventory, (reason) => setError(String(reason)));
  }, []);

  return (
    <div className="page">
      <PageHeader title="Inventory" subtitle="Declared hosts, clusters, and inert discovery records." />
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
  const declared = inventory.hosts.filter(
    (host) => host.status === 'approved' || (host.status === undefined && host.cluster === undefined),
  );
  const inert = inventory.hosts.filter((host) => host.status === 'draft' || host.status === 'ignored');

  return (
    <>
      <Panel>
        <PanelHeader aside={`${declared.length} hosts`}>Declared hosts</PanelHeader>
        {declared.length === 0 ? (
          <EmptyState titleText="No declared hosts" headingLevel="h2" variant="sm">
            <EmptyStateBody>Declared inventory entries will appear here.</EmptyStateBody>
          </EmptyState>
        ) : (
          <Table variant="compact" aria-label="Declared hosts">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Hostname</Th>
                <Th>Aliases</Th>
                <Th>Connection</Th>
                <Th>State</Th>
              </Tr>
            </Thead>
            <Tbody>
              {declared.map((host) => (
                <Tr key={host.name}>
                  <Td dataLabel="Name"><span className="mono">{host.name}</span></Td>
                  <Td dataLabel="Hostname">{valueOrDash(host.hostname)}</Td>
                  <Td dataLabel="Aliases">{host.aliases?.join(', ') || '—'}</Td>
                  <Td dataLabel="Connection">{host.connection}</Td>
                  <Td dataLabel="State">
                    <Label color="green" isCompact>
                      {host.status === 'approved' ? 'approved' : 'declared'}
                    </Label>
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </Panel>

      <Panel>
        <PanelHeader aside={`${inventory.clusters.length} clusters`}>Clusters</PanelHeader>
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
                    <Td dataLabel="Type">
                      <Label color="blue" isCompact>{cluster.type}</Label>
                    </Td>
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
      </Panel>

      <Panel className="inert-panel">
        <PanelHeader aside={`${inert.length} records`}>Inert</PanelHeader>
        {inert.length === 0 ? (
          <div className="panel__body">No draft or ignored discovery records.</div>
        ) : (
          <Table variant="compact" aria-label="Inert inventory records">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Hostname</Th>
                <Th>Seen via</Th>
                <Th>First seen</Th>
                <Th>State</Th>
              </Tr>
            </Thead>
            <Tbody>
              {inert.map((host) => (
                <Tr key={host.name}>
                  <Td dataLabel="Name">{host.name}</Td>
                  <Td dataLabel="Hostname">{valueOrDash(host.hostname)}</Td>
                  <Td dataLabel="Seen via">{valueOrDash(host.source)}</Td>
                  <Td dataLabel="First seen">
                    {host.first_seen === undefined ? '—' : new Date(host.first_seen).toLocaleString()}
                  </Td>
                  <Td dataLabel="State">
                    <Label color="grey" isCompact>
                      {host.status === 'draft' ? 'draft · proposal' : 'ignored · dismissed'}
                    </Label>
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </Panel>
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
