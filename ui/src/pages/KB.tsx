import { useEffect, useState } from 'react';
import { Alert, Button, Card, CardBody, Label, PageSection, Spinner, Title } from '@patternfly/react-core';
import { Table, Thead, Tbody, Tr, Th, Td } from '@patternfly/react-table';
import { api, type KBArticleMeta } from '../api';
import { VerdictLabel } from './Investigations';

function StatusBadge({ article }: { article: KBArticleMeta }) {
  return (
    <>
      <Label color={article.status === 'approved' ? 'green' : 'grey'}>{article.status}</Label>{' '}
      {article.nominated === true && article.status === 'draft' && <Label color="purple">nominated</Label>}
    </>
  );
}

function Article({ slug, onBack }: { slug: string; onBack: () => void }) {
  const [markdown, setMarkdown] = useState<string | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    api.getKBArticle(slug).then(setMarkdown, (e) => setError(String(e)));
  }, [slug]);

  return (
    <PageSection>
      <Button variant="link" onClick={onBack} style={{ paddingLeft: 0 }}>
        ← All articles
      </Button>
      {error !== '' && <Alert variant="danger" title={error} />}
      {markdown === null && error === '' ? (
        <Spinner />
      ) : (
        <Card style={{ margin: '1rem 0' }}>
          <CardBody>
            <pre style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontFamily: 'inherit' }}>{markdown}</pre>
          </CardBody>
        </Card>
      )}
    </PageSection>
  );
}

export function KB() {
  const [articles, setArticles] = useState<KBArticleMeta[] | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    if (selected !== null) return;
    api.listKB().then(setArticles, (e) => setError(String(e)));
  }, [selected]);

  if (selected !== null) return <Article slug={selected} onBack={() => setSelected(null)} />;

  if (error !== '') {
    return (
      <PageSection>
        <Alert variant="danger" title={error} />
      </PageSection>
    );
  }
  if (articles === null) {
    return (
      <PageSection>
        <Spinner />
      </PageSection>
    );
  }
  return (
    <PageSection>
      <Title headingLevel="h2">Knowledge base</Title>
      {articles.length === 0 ? (
        <p style={{ margin: '1rem 0' }}>No articles yet — they are drafted from investigations that end in a verdict.</p>
      ) : (
        <Table variant="compact" aria-label="Knowledge-base articles" style={{ margin: '1rem 0' }}>
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
              <Tr key={article.slug} isClickable onRowClick={() => setSelected(article.slug)}>
                <Td>{article.title}</Td>
                <Td>
                  <StatusBadge article={article} />
                </Td>
                <Td>{article.verdict !== '' ? <VerdictLabel verdict={article.verdict} /> : '—'}</Td>
                <Td>{article.occurrences.length}</Td>
                <Td>{article.resolution.verified}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      )}
    </PageSection>
  );
}
