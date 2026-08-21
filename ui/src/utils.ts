import type { SecretRef } from './api';

export function relativeTime(value: string): string {
  const seconds = Math.round((new Date(value).getTime() - Date.now()) / 1000);
  const absolute = Math.abs(seconds);
  if (absolute < 45) return 'just now';
  const formatter = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
  if (absolute < 3600) return formatter.format(Math.round(seconds / 60), 'minute');
  if (absolute < 86400) return formatter.format(Math.round(seconds / 3600), 'hour');
  if (absolute < 2592000) return formatter.format(Math.round(seconds / 86400), 'day');
  return formatter.format(Math.round(seconds / 2592000), 'month');
}

export function secretRefText(secret: SecretRef): string {
  return 'env' in secret ? `env:${secret.env}` : `file:${secret.file}`;
}

export function endpointHost(value: string): string {
  try {
    return new URL(value).host;
  } catch {
    return value;
  }
}
