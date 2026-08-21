import type { ReactNode } from 'react';
import { Label, Spinner, Title } from '@patternfly/react-core';
import {
  CheckCircleIcon,
  ExclamationCircleIcon,
  ExclamationTriangleIcon,
  LockIcon,
} from '@patternfly/react-icons';
import type { SecretRef } from './api';

export function PageHeader({
  title,
  subtitle,
  eyebrow,
}: {
  title: string;
  subtitle: ReactNode;
  eyebrow?: ReactNode;
}) {
  return (
    <header className="page-header">
      {eyebrow !== undefined && <div className="page-header__eyebrow">{eyebrow}</div>}
      <Title headingLevel="h1">{title}</Title>
      <div className="page-header__subtitle">{subtitle}</div>
    </header>
  );
}

export function Panel({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <section className={`panel ${className}`}>{children}</section>;
}

export function PanelHeader({ children, aside }: { children: ReactNode; aside?: ReactNode }) {
  return (
    <div className="panel__header">
      <span>{children}</span>
      {aside !== undefined && <span className="panel__header-aside">{aside}</span>}
    </div>
  );
}

export function StatusLabel({ status }: { status: string }) {
  const color = status === 'completed' ? 'green' : status === 'failed' ? 'red' : 'blue';
  return (
    <Label color={color} isCompact>
      {status}
    </Label>
  );
}

export function VerdictLabel({ verdict }: { verdict: string }) {
  if (verdict === 'healthy') {
    return (
      <Label color="green" isCompact icon={<CheckCircleIcon />}>
        healthy
      </Label>
    );
  }
  if (verdict === 'attention') {
    return (
      <Label color="yellow" isCompact icon={<ExclamationTriangleIcon />}>
        attention
      </Label>
    );
  }
  return (
    <Label color="orangered" isCompact icon={<ExclamationCircleIcon />}>
      action
    </Label>
  );
}

export function SecretRefView({ secret }: { secret?: SecretRef }) {
  if (secret === undefined) return <>—</>;
  const value = 'env' in secret ? `env: ${secret.env}` : `file: ${secret.file}`;
  return (
    <span className="inline-cluster">
      <span className="mono mono--small">{value}</span>
      <Label color="grey" isCompact icon={<LockIcon />}>
        reference only
      </Label>
    </span>
  );
}

export function PageLoading() {
  return (
    <div className="page-loading" aria-label="Loading">
      <Spinner />
    </div>
  );
}
