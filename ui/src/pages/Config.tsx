import { useEffect, useState } from 'react';
import {
  Alert,
  Card,
  CardBody,
  CardTitle,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  PageSection,
  Spinner,
  Title,
} from '@patternfly/react-core';
import { api, type ConfigView } from '../api';

function ref(secret: { env?: string; file?: string } | undefined): string {
  if (secret === undefined) return '—';
  if (secret.env !== undefined) return `env: ${secret.env}`;
  if (secret.file !== undefined) return `file: ${secret.file}`;
  return '—';
}

export function Config() {
  const [config, setConfig] = useState<ConfigView | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    api.getConfig().then(setConfig, (e) => setError(String(e)));
  }, []);

  if (error !== '') {
    return (
      <PageSection>
        <Alert variant="danger" title={error} />
      </PageSection>
    );
  }
  if (config === null) {
    return (
      <PageSection>
        <Spinner />
      </PageSection>
    );
  }
  return (
    <PageSection>
      <Title headingLevel="h2">Configuration</Title>
      <Card style={{ margin: '1rem 0' }}>
        <CardTitle>Daemon</CardTitle>
        <CardBody>
          <DescriptionList isHorizontal>
            <DescriptionListGroup>
              <DescriptionListTerm>Listen</DescriptionListTerm>
              <DescriptionListDescription>{config.listen}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>State dir</DescriptionListTerm>
              <DescriptionListDescription>{config.state_dir}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>API token</DescriptionListTerm>
              <DescriptionListDescription>{ref(config.api_token)}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>Gated tools</DescriptionListTerm>
              <DescriptionListDescription>{config.gated_tools.join(', ')}</DescriptionListDescription>
            </DescriptionListGroup>
          </DescriptionList>
        </CardBody>
      </Card>
      <Card style={{ margin: '1rem 0' }}>
        <CardTitle>Model</CardTitle>
        <CardBody>
          <DescriptionList isHorizontal>
            <DescriptionListGroup>
              <DescriptionListTerm>Base URL</DescriptionListTerm>
              <DescriptionListDescription>{config.model.base_url}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>Name</DescriptionListTerm>
              <DescriptionListDescription>{config.model.name}</DescriptionListDescription>
            </DescriptionListGroup>
            <DescriptionListGroup>
              <DescriptionListTerm>API key</DescriptionListTerm>
              <DescriptionListDescription>{ref(config.model.api_key)}</DescriptionListDescription>
            </DescriptionListGroup>
          </DescriptionList>
        </CardBody>
      </Card>
      <Card style={{ margin: '1rem 0' }}>
        <CardTitle>MCP servers</CardTitle>
        <CardBody>
          <DescriptionList isHorizontal>
            {config.mcp_servers.map((server) => (
              <DescriptionListGroup key={server.name}>
                <DescriptionListTerm>{server.name}</DescriptionListTerm>
                <DescriptionListDescription>
                  {server.url}
                  {server.auth_header !== undefined ? ` (${ref(server.auth_header)})` : ''}
                </DescriptionListDescription>
              </DescriptionListGroup>
            ))}
          </DescriptionList>
        </CardBody>
      </Card>
      <Card style={{ margin: '1rem 0' }}>
        <CardTitle>Sweeps</CardTitle>
        <CardBody>
          {config.sweeps.length === 0 ? (
            'None configured'
          ) : (
            <DescriptionList isHorizontal>
              {config.sweeps.map((sweep) => (
                <DescriptionListGroup key={sweep.name}>
                  <DescriptionListTerm>
                    {sweep.name} ({sweep.schedule})
                  </DescriptionListTerm>
                  <DescriptionListDescription>{sweep.question}</DescriptionListDescription>
                </DescriptionListGroup>
              ))}
            </DescriptionList>
          )}
        </CardBody>
      </Card>
    </PageSection>
  );
}
