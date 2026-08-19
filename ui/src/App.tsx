import { useState } from 'react';
import {
  Button,
  Card,
  CardBody,
  Form,
  FormGroup,
  Masthead,
  MastheadContent,
  MastheadMain,
  Nav,
  NavItem,
  NavList,
  Page,
  PageSection,
  PageSidebar,
  PageSidebarBody,
  TextInput,
  Title,
} from '@patternfly/react-core';
import { clearToken, getToken, setToken } from './api';
import { Investigations } from './pages/Investigations';
import { Approvals } from './pages/Approvals';
import { Config } from './pages/Config';

type Section = 'investigations' | 'approvals' | 'config';

function TokenGate({ onSet }: { onSet: () => void }) {
  const [value, setValue] = useState('');
  return (
    <Page>
      <PageSection>
        <Card style={{ maxWidth: 480, margin: '10vh auto' }}>
          <CardBody>
            <Title headingLevel="h1">Shackleton</Title>
            <Form
              onSubmit={(e) => {
                e.preventDefault();
                if (value.trim() !== '') {
                  setToken(value.trim());
                  onSet();
                }
              }}
            >
              <FormGroup label="API token" fieldId="token">
                <TextInput
                  id="token"
                  type="password"
                  value={value}
                  onChange={(_e, v) => setValue(v)}
                  aria-label="API token"
                />
              </FormGroup>
              <Button type="submit" variant="primary">
                Connect
              </Button>
            </Form>
          </CardBody>
        </Card>
      </PageSection>
    </Page>
  );
}

export function App() {
  const [authed, setAuthed] = useState(getToken() !== null);
  const [section, setSection] = useState<Section>('investigations');

  if (!authed) return <TokenGate onSet={() => setAuthed(true)} />;

  const masthead = (
    <Masthead>
      <MastheadMain>
        <Title headingLevel="h1" style={{ padding: '0.5rem 1rem' }}>
          Shackleton
        </Title>
      </MastheadMain>
      <MastheadContent>
        <Button
          variant="link"
          onClick={() => {
            clearToken();
            setAuthed(false);
          }}
        >
          Disconnect
        </Button>
      </MastheadContent>
    </Masthead>
  );

  const sidebar = (
    <PageSidebar>
      <PageSidebarBody>
        <Nav>
          <NavList>
            <NavItem itemId="investigations" isActive={section === 'investigations'} onClick={() => setSection('investigations')}>
              Investigations
            </NavItem>
            <NavItem itemId="approvals" isActive={section === 'approvals'} onClick={() => setSection('approvals')}>
              Approvals
            </NavItem>
            <NavItem itemId="config" isActive={section === 'config'} onClick={() => setSection('config')}>
              Config
            </NavItem>
          </NavList>
        </Nav>
      </PageSidebarBody>
    </PageSidebar>
  );

  return (
    <Page masthead={masthead} sidebar={sidebar}>
      {section === 'investigations' && <Investigations />}
      {section === 'approvals' && <Approvals />}
      {section === 'config' && <Config />}
    </Page>
  );
}
