// Package kb persists resolution records: one KCS-shaped markdown article per
// symptom, front-matter for machines, prose for humans. Articles start as
// machine-owned drafts; once an operator sets status: blessed the body is
// theirs and the system only ever appends occurrence metadata.
package kb

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Symptom struct {
	Trigger      string   `yaml:"trigger" json:"trigger"`
	Alertname    string   `yaml:"alertname,omitempty" json:"alertname,omitempty"`
	Fingerprints []string `yaml:"fingerprints,omitempty" json:"fingerprints,omitempty"`
	Sweep        string   `yaml:"sweep,omitempty" json:"sweep,omitempty"`
}

type Occurrence struct {
	Investigation string    `yaml:"investigation" json:"investigation"`
	At            time.Time `yaml:"at" json:"at"`
}

type Action struct {
	Human   string `yaml:"human" json:"human"`
	Outcome string `yaml:"outcome" json:"outcome"`
}

type Resolution struct {
	Actions  []Action `yaml:"actions" json:"actions"`
	Verified string   `yaml:"verified" json:"verified"`
}

type FrontMatter struct {
	Slug        string       `yaml:"slug" json:"slug"`
	Title       string       `yaml:"title" json:"title"`
	Status      string       `yaml:"status" json:"status"`
	Symptom     Symptom      `yaml:"symptom" json:"symptom"`
	Verdict     string       `yaml:"verdict" json:"verdict"`
	Occurrences []Occurrence `yaml:"occurrences" json:"occurrences"`
	Resolution  Resolution   `yaml:"resolution" json:"resolution"`
}

type Article struct {
	FrontMatter
	Body string
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

const delimiter = "---\n"

// Record writes or merges an article. A new slug becomes a draft. An existing
// draft is rewritten wholesale (latest investigation wins). A blessed article
// keeps its body and prose untouched; only occurrences, fingerprints, and
// resolution metadata are merged into the front-matter.
func (s *Store) Record(article Article) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(article.Slug)
	existing, err := readArticle(path)
	if err == nil {
		merged := mergeMeta(existing.FrontMatter, article.FrontMatter)
		if existing.Status == "blessed" {
			merged.Status = "blessed"
			merged.Title = existing.Title
			return writeArticle(path, Article{FrontMatter: merged, Body: existing.Body})
		}
		return writeArticle(path, Article{FrontMatter: merged, Body: article.Body})
	}
	if !os.IsNotExist(err) {
		return err
	}
	article.Status = "draft"
	if article.Resolution.Verified == "" {
		article.Resolution.Verified = "none"
	}
	return writeArticle(path, article)
}

func mergeMeta(existing, incoming FrontMatter) FrontMatter {
	merged := incoming
	merged.Occurrences = append(existing.Occurrences, incoming.Occurrences...)
	have := make(map[string]bool, len(existing.Symptom.Fingerprints))
	fingerprints := append([]string{}, existing.Symptom.Fingerprints...)
	for _, fp := range fingerprints {
		have[fp] = true
	}
	for _, fp := range incoming.Symptom.Fingerprints {
		if !have[fp] {
			fingerprints = append(fingerprints, fp)
		}
	}
	merged.Symptom.Fingerprints = fingerprints
	merged.Status = existing.Status
	if incoming.Resolution.Verified == "" {
		merged.Resolution.Verified = existing.Resolution.Verified
	}
	if merged.Resolution.Verified == "" {
		merged.Resolution.Verified = "none"
	}
	return merged
}

func (s *Store) List() ([]FrontMatter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	result := []FrontMatter{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		article, err := readArticle(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}
		result = append(result, article.FrontMatter)
	}
	return result, nil
}

func (s *Store) Get(slug string) ([]byte, error) {
	if slug != filepath.Base(slug) || strings.Contains(slug, "..") {
		return nil, os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.ReadFile(s.path(slug))
}

func (s *Store) path(slug string) string {
	return filepath.Join(s.dir, slug+".md")
}

func readArticle(path string) (Article, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Article{}, err
	}
	content := string(raw)
	if !strings.HasPrefix(content, delimiter) {
		return Article{}, fmt.Errorf("%s: missing front-matter", path)
	}
	meta, body, ok := strings.Cut(content[len(delimiter):], delimiter)
	if !ok {
		return Article{}, fmt.Errorf("%s: unterminated front-matter", path)
	}
	var front FrontMatter
	if err := yaml.Unmarshal([]byte(meta), &front); err != nil {
		return Article{}, fmt.Errorf("%s: %w", path, err)
	}
	return Article{FrontMatter: front, Body: body}, nil
}

func writeArticle(path string, article Article) error {
	var buf bytes.Buffer
	buf.WriteString(delimiter)
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(article.FrontMatter); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	buf.WriteString(delimiter)
	buf.WriteString(article.Body)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
