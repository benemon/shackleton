import { useEffect, useState } from 'react';
import { Alert, EmptyState, EmptyStateBody, Label } from '@patternfly/react-core';
import { CheckCircleIcon } from '@patternfly/react-icons';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { api, type KBArticleMeta } from '../api';
import { PageHeader, PageLoading, Panel, PanelHeader, VerdictLabel } from '../components';

function ArticleStatus({ article }: { article: KBArticleMeta }) {
  return (
    <span className="inline-cluster">
      <Label
        color={article.status === 'approved' ? 'green' : 'grey'}
        isCompact
        icon={article.status === 'approved' ? <CheckCircleIcon /> : undefined}
      >
        {article.status}
      </Label>
      {article.status === 'draft' && article.nominated === true && (
        <Label color="purple" isCompact>
          nominated
        </Label>
      )}
    </span>
  );
}

function VerifiedLabel({ value }: { value?: string }) {
  const label = value === undefined || value === '' || value === 'none' ? 'unverified' : value;
  const color = label === 'cleared' ? 'green' : label === 'persisting' ? 'yellow' : 'grey';
  return (
    <Label color={color} isCompact>
      {label}
    </Label>
  );
}

type MarkdownBlock =
  | { kind: 'h1' | 'h2' | 'p' | 'code'; text: string }
  | { kind: 'li'; marker: string; text: string };

function stripFrontMatter(markdown: string): string {
  const lines = markdown.split('\n');
  if (lines[0]?.trim() !== '---') return markdown;
  const end = lines.findIndex((line, index) => index > 0 && line.trim() === '---');
  return end < 0 ? markdown : lines.slice(end + 1).join('\n').trimStart();
}

function parseMarkdown(markdown: string): MarkdownBlock[] {
  const output: MarkdownBlock[] = [];
  let fence: string[] | null = null;
  for (const line of markdown.split('\n')) {
    if (line.trim().startsWith('```')) {
      if (fence === null) fence = [];
      else {
        output.push({ kind: 'code', text: fence.join('\n') });
        fence = null;
      }
      continue;
    }
    if (fence !== null) {
      fence.push(line);
      continue;
    }
    if (line.startsWith('## ')) output.push({ kind: 'h2', text: line.slice(3) });
    else if (line.startsWith('# ')) output.push({ kind: 'h1', text: line.slice(2) });
    else {
      const ordered = line.match(/^(\d+)\.\s+(.*)$/);
      if (ordered !== null) output.push({ kind: 'li', marker: `${ordered[1]}.`, text: ordered[2] });
      else if (line.startsWith('- ')) output.push({ kind: 'li', marker: '—', text: line.slice(2) });
      else if (line.trim() !== '') output.push({ kind: 'p', text: line });
    }
  }
  if (fence !== null) output.push({ kind: 'code', text: fence.join('\n') });
  return output;
}

function Markdown({ source }: { source: string }) {
  return (
    <>
      {parseMarkdown(stripFrontMatter(source)).map((block, index) => {
        if (block.kind === 'h1') return <h1 key={index}>{block.text}</h1>;
        if (block.kind === 'h2') return <h2 key={index}>{block.text}</h2>;
        if (block.kind === 'code') return <pre className="mono-pre" key={index}>{block.text}</pre>;
        if (block.kind === 'li') {
          return (
            <div className="markdown-list-item" key={index}>
              <span className="mono">{block.marker}</span>
              <span>{block.text}</span>
            </div>
          );
        }
        return <p key={index}>{block.text}</p>;
      })}
    </>
  );
}

export function KBArticle() {
  const { slug = '' } = useParams();
  const [article, setArticle] = useState<KBArticleMeta | null>(null);
  const [markdown, setMarkdown] = useState<string | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    Promise.all([api.listKB(), api.getKBArticle(slug)]).then(
      ([articles, source]) => {
        const match = articles.find((candidate) => candidate.slug === slug);
        if (match === undefined) {
          setError('Article metadata was not found.');
          return;
        }
        setArticle(match);
        setMarkdown(source);
      },
      (reason) => setError(String(reason)),
    );
  }, [slug]);

  const resolutionLine =
    article?.resolution.verified === 'cleared'
      ? 'The recorded resolution cleared the symptom.'
      : article?.resolution.verified === 'persisting'
        ? 'The symptom persisted after the recorded resolution.'
        : 'The recorded resolution has not been verified yet.';

  return (
    <div className="page">
      {error !== '' && (
        <Alert variant="danger" title="Could not load the article">
          {error}
        </Alert>
      )}
      {article === null || markdown === null ? (
        error === '' && <PageLoading />
      ) : (
        <>
          <PageHeader
            title={article.title}
            subtitle={
              <span className="inline-cluster">
                <ArticleStatus article={article} />
                {article.verdict !== '' && <VerdictLabel verdict={article.verdict} />}
                <span>{article.occurrences.length} {article.occurrences.length === 1 ? 'occurrence' : 'occurrences'}</span>
              </span>
            }
            eyebrow={<Link to="/kb">← All articles</Link>}
          />
          <div className="article-layout">
            <Panel>
              <article className="article-content">
                <Markdown source={markdown} />
              </article>
            </Panel>
            <aside className="stack">
              <Panel>
                <PanelHeader>Occurrences</PanelHeader>
                <div className="panel__body">
                  {article.occurrences.length === 0 ? (
                    <span className="subtle">No linked investigations.</span>
                  ) : (
                    article.occurrences.map((occurrence, index) => (
                      <div className="item-row" key={`${occurrence.investigation}-${index}`}>
                        <div className="item-row__content stack stack--tight">
                          <Link
                            className="mono mono--small"
                            to={`/investigations/${encodeURIComponent(occurrence.investigation)}`}
                          >
                            {occurrence.investigation}
                          </Link>
                          <span className="subtle">{new Date(occurrence.at).toLocaleString()}</span>
                          <VerifiedLabel value={occurrence.verified} />
                        </div>
                      </div>
                    ))
                  )}
                </div>
              </Panel>
              <Panel>
                <PanelHeader>Resolution</PanelHeader>
                <div className="panel__body stack">
                  <VerifiedLabel value={article.resolution.verified} />
                  <p>{resolutionLine}</p>
                </div>
              </Panel>
            </aside>
          </div>
        </>
      )}
    </div>
  );
}

export function KB() {
  const navigate = useNavigate();
  const [articles, setArticles] = useState<KBArticleMeta[] | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    api.listKB().then(setArticles, (reason) => setError(String(reason)));
  }, []);

  return (
    <div className="page">
      <PageHeader title="Knowledge base" subtitle="Operator-reviewed findings and resolutions from past investigations." />
      {error !== '' && (
        <Alert variant="danger" title="Could not load the knowledge base">
          {error}
        </Alert>
      )}
      <Panel>
        {articles === null ? (
          error === '' && <PageLoading />
        ) : articles.length === 0 ? (
          <EmptyState titleText="No articles yet" headingLevel="h2" variant="sm">
            <EmptyStateBody>Articles are drafted from investigations that end in a verdict.</EmptyStateBody>
          </EmptyState>
        ) : (
          <Table variant="compact" aria-label="Knowledge-base articles">
            <Thead>
              <Tr>
                <Th>Title</Th>
                <Th>Status</Th>
                <Th>Verdict</Th>
                <Th>Occurrences</Th>
                <Th>Resolution verified</Th>
              </Tr>
            </Thead>
            <Tbody>
              {articles.map((article) => (
                <Tr key={article.slug} isClickable onRowClick={() => navigate(`/kb/${encodeURIComponent(article.slug)}`)}>
                  <Td dataLabel="Title">
                    <Link className="table-link" to={`/kb/${encodeURIComponent(article.slug)}`}>
                      {article.title}
                    </Link>
                  </Td>
                  <Td dataLabel="Status">
                    <ArticleStatus article={article} />
                  </Td>
                  <Td dataLabel="Verdict">
                    {article.verdict === '' ? '—' : <VerdictLabel verdict={article.verdict} />}
                  </Td>
                  <Td dataLabel="Occurrences">{article.occurrences.length}</Td>
                  <Td dataLabel="Resolution verified">
                    <VerifiedLabel value={article.resolution.verified} />
                  </Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        )}
      </Panel>
    </div>
  );
}
