import { useEffect, useState } from 'react';
import {
  Badge,
  Button,
  Form,
  FormGroup,
  Nav,
  NavExpandable,
  NavItem,
  NavList,
  Spinner,
  TextInput,
  Title,
} from '@patternfly/react-core';
import {
  BellIcon,
  BookIcon,
  CogIcon,
  CubesIcon,
  HomeAltIcon,
  SearchIcon,
} from '@patternfly/react-icons';
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { api, clearToken, session, setToken, type ConfigView, type Health } from './api';
import { Panel } from './components';
import { AdminChannels, AdminKnowledge, AdminMetrics, AdminPlatform, AdminSweeps, AdminTools } from './pages/Admin';
import { Approvals } from './pages/Approvals';
import { InvestigationDetail, Investigations } from './pages/Investigations';
import { Inventory } from './pages/Inventory';
import { KB, KBArticle } from './pages/KB';
import { Overview } from './pages/Overview';

export function TokenGate({ onSet }: { onSet: () => void }) {
  const [value, setValue] = useState('');
  return (
    <main className="token-gate">
      <Panel className="token-gate__panel">
        <div className="panel__body stack">
          <div>
            <Title headingLevel="h1">Shackleton</Title>
            <p className="subtle">Connect to the daemon console.</p>
          </div>
          <Form
            onSubmit={(event) => {
              event.preventDefault();
              const token = value.trim();
              if (token !== '') {
                setToken(token);
                // Fire-and-forget: the cookie only matters on the next reload.
                session.create(token).catch(() => undefined);
                onSet();
              }
            }}
          >
            <FormGroup label="API token" fieldId="token">
              <TextInput
                id="token"
                type="password"
                value={value}
                onChange={(_event, next) => setValue(next)}
                aria-label="API token"
              />
            </FormGroup>
            <Button type="submit" variant="primary">
              Connect
            </Button>
          </Form>
        </div>
      </Panel>
    </main>
  );
}

type NavEntry = { path: string; label: string; icon?: React.ReactNode };

const primaryNavigation: NavEntry[] = [
  { path: '/', label: 'Overview', icon: <HomeAltIcon /> },
  { path: '/investigations', label: 'Investigations', icon: <SearchIcon /> },
  { path: '/approvals', label: 'Approvals', icon: <BellIcon /> },
  { path: '/kb', label: 'Knowledge base', icon: <BookIcon /> },
  { path: '/inventory', label: 'Inventory', icon: <CubesIcon /> },
];

const administrationNavigation: NavEntry[] = [
  { path: '/admin/platform', label: 'Platform' },
  { path: '/admin/tools', label: 'Tool servers' },
  { path: '/admin/metrics', label: 'Metrics sources' },
  { path: '/admin/knowledge', label: 'Knowledge sources' },
  { path: '/admin/channels', label: 'Channels' },
  { path: '/admin/sweeps', label: 'Sweeps' },
];

export function Console() {
  const location = useLocation();
  const navigate = useNavigate();
  const [health, setHealth] = useState<Health | null>(null);
  const [config, setConfig] = useState<ConfigView | null>(null);
  const [approvalCount, setApprovalCount] = useState(0);
  const [administrationExpanded, setAdministrationExpanded] = useState(true);

  useEffect(() => {
    const refreshApprovals = () => api.listApprovals().then((items) => setApprovalCount(items.length), () => undefined);
    api.getHealth().then(setHealth, () => undefined);
    api.getConfig().then(setConfig, () => undefined);
    refreshApprovals();
    const timer = window.setInterval(refreshApprovals, 5000);
    return () => window.clearInterval(timer);
  }, []);

  const isActive = (path: string) =>
    path === '/'
      ? location.pathname === '/'
      : location.pathname === path || location.pathname.startsWith(`${path}/`);

  return (
    <div className="console-shell">
      <header className="console-header">
        <div className="console-wordmark">Shackleton</div>
        <div className="console-daemon">
          <span className={`status-dot ${health?.status === 'ok' ? 'status-dot--ok' : ''}`} />
          <span>
            {health?.status ?? 'connecting'} · {config?.model.name ?? 'loading model'}
          </span>
        </div>
        <div className="console-header__actions">
          <Button
            variant="link"
            onClick={() => {
              session.end().finally(() => {
                clearToken();
                window.location.reload();
              });
            }}
          >
            Disconnect
          </Button>
        </div>
      </header>
      <aside className="console-sidebar">
        <Nav aria-label="Console navigation">
          <NavList>
            {primaryNavigation.map((entry) => (
              <NavItem
                key={entry.path}
                itemId={entry.path}
                isActive={isActive(entry.path)}
                onClick={() => navigate(entry.path)}
                icon={entry.icon}
              >
                <span className="nav-item-content">
                  {entry.label}
                  {entry.path === '/approvals' && approvalCount > 0 && <Badge>{approvalCount}</Badge>}
                </span>
              </NavItem>
            ))}
            <NavExpandable
              title="Administration"
              icon={<CogIcon />}
              isExpanded={administrationExpanded}
              isActive={location.pathname.startsWith('/admin/')}
              onExpand={(_event, expanded) => setAdministrationExpanded(expanded)}
            >
              {administrationNavigation.map((entry) => (
                <NavItem
                  key={entry.path}
                  itemId={entry.path}
                  isActive={isActive(entry.path)}
                  onClick={() => navigate(entry.path)}
                >
                  {entry.label}
                </NavItem>
              ))}
            </NavExpandable>
          </NavList>
        </Nav>
      </aside>
      <main className="console-main">
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/investigations" element={<Investigations />} />
          <Route path="/investigations/:id" element={<InvestigationDetail />} />
          <Route path="/approvals" element={<Approvals />} />
          <Route path="/kb" element={<KB />} />
          <Route path="/kb/:slug" element={<KBArticle />} />
          <Route path="/inventory" element={<Inventory />} />
          <Route path="/admin/platform" element={<AdminPlatform />} />
          <Route path="/admin/tools" element={<AdminTools />} />
          <Route path="/admin/metrics" element={<AdminMetrics />} />
          <Route path="/admin/knowledge" element={<AdminKnowledge />} />
          <Route path="/admin/channels" element={<AdminChannels />} />
          <Route path="/admin/sweeps" element={<AdminSweeps />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}

export function App() {
  const [phase, setPhase] = useState<'restoring' | 'gate' | 'console'>('restoring');
  useEffect(() => {
    session.restore().then(
      (restored) => {
        if (restored !== null) setToken(restored);
        setPhase(restored !== null ? 'console' : 'gate');
      },
      () => setPhase('gate'),
    );
  }, []);
  if (phase === 'restoring') {
    return (
      <main className="token-gate">
        <div className="stack" style={{ alignItems: 'center' }}>
          <Spinner aria-label="Restoring your session…" />
          <p className="subtle">Restoring your session…</p>
        </div>
      </main>
    );
  }
  if (phase === 'gate') return <TokenGate onSet={() => setPhase('console')} />;
  return <Console />;
}
