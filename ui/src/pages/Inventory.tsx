import { useEffect, useState } from 'react';
import { Alert, Card, CardBody, CardTitle, PageSection, Spinner, Title } from '@patternfly/react-core';
import { Table, Thead, Tbody, Tr, Th, Td } from '@patternfly/react-table';
import { api, type Inventory as InventoryView } from '../api';

export function Inventory() {
  const [inventory, setInventory] = useState<InventoryView | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    api.getInventory().then(setInventory, (e) => setError(String(e)));
  }, []);

  if (error !== '') {
    return (
      <PageSection>
        <Alert variant="danger" title={error} />
      </PageSection>
    );
  }
  if (inventory === null) {
    return (
      <PageSection>
        <Spinner />
      </PageSection>
    );
  }
  return (
    <PageSection>
      <Title headingLevel="h2">Inventory</Title>
      <Card style={{ margin: '1rem 0' }}>
        <CardTitle>Hosts</CardTitle>
        <CardBody>
          {inventory.hosts.length === 0 ? (
            'None declared'
          ) : (
            <Table variant="compact" aria-label="Hosts">
              <Thead>
                <Tr>
                  <Th>Name</Th>
                  <Th>Hostname</Th>
                  <Th>Aliases</Th>
                  <Th>Connection</Th>
                </Tr>
              </Thead>
              <Tbody>
                {inventory.hosts.map((host) => (
                  <Tr key={host.name}>
                    <Td>{host.name}</Td>
                    <Td>{host.hostname ?? '—'}</Td>
                    <Td>{host.aliases !== undefined && host.aliases.length > 0 ? host.aliases.join(', ') : '—'}</Td>
                    <Td>{host.connection}</Td>
                  </Tr>
                ))}
              </Tbody>
            </Table>
          )}
        </CardBody>
      </Card>
      <Card style={{ margin: '1rem 0' }}>
        <CardTitle>Clusters</CardTitle>
        <CardBody>
          {inventory.clusters.length === 0 ? (
            'None declared'
          ) : (
            <Table variant="compact" aria-label="Clusters">
              <Thead>
                <Tr>
                  <Th>Name</Th>
                  <Th>Type</Th>
                  <Th>API</Th>
                </Tr>
              </Thead>
              <Tbody>
                {inventory.clusters.map((cluster) => (
                  <Tr key={cluster.name}>
                    <Td>{cluster.name}</Td>
                    <Td>{cluster.type}</Td>
                    <Td>{cluster.api}</Td>
                  </Tr>
                ))}
              </Tbody>
            </Table>
          )}
        </CardBody>
      </Card>
    </PageSection>
  );
}
