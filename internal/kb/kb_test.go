package kb

import (
	"os"
	"strings"
	"testing"
	"time"
)

func article(slug, title, body string, fingerprint string) Article {
	return Article{FrontMatter: FrontMatter{
		Slug: slug, Title: title, Verdict: "attention",
		Symptom:     Symptom{Trigger: "alert", Alertname: "TestAlert", Fingerprints: []string{fingerprint}},
		Occurrences: []Occurrence{{Investigation: "inv-" + fingerprint, At: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}},
		Resolution:  Resolution{Actions: []Action{}, Verified: ""},
	}, Body: body}
}

func TestRecordCreatesDraftAndListsIt(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(article("alert-testalert", "TestAlert (alert)", "# body one\n", "fp1")); err != nil {
		t.Fatal(err)
	}
	articles, err := store.List()
	if err != nil || len(articles) != 1 {
		t.Fatalf("list = %+v, %v", articles, err)
	}
	got := articles[0]
	if got.Status != "draft" || got.Resolution.Verified != "none" || got.Symptom.Alertname != "TestAlert" {
		t.Fatalf("front-matter = %+v", got)
	}
	raw, err := store.Get("alert-testalert")
	if err != nil || !strings.Contains(string(raw), "# body one") {
		t.Fatalf("get = %q, %v", raw, err)
	}
}

func TestDraftMergeRewritesBodyAndAccumulatesOccurrences(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(article("a", "T", "old body\n", "fp1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(article("a", "T2", "new body\n", "fp2")); err != nil {
		t.Fatal(err)
	}
	articles, _ := store.List()
	got := articles[0]
	if len(got.Occurrences) != 2 || len(got.Symptom.Fingerprints) != 2 || got.Status != "draft" || got.Title != "T2" {
		t.Fatalf("merged = %+v", got)
	}
	raw, _ := store.Get("a")
	if !strings.Contains(string(raw), "new body") || strings.Contains(string(raw), "old body") {
		t.Fatalf("draft body not rewritten: %q", raw)
	}
	// Same fingerprint again must not duplicate.
	if err := store.Record(article("a", "T3", "x\n", "fp2")); err != nil {
		t.Fatal(err)
	}
	articles, _ = store.List()
	if len(articles[0].Symptom.Fingerprints) != 2 {
		t.Fatalf("fingerprints duplicated: %+v", articles[0].Symptom.Fingerprints)
	}
}

func TestApprovedBodyAndTitleAreNeverTouched(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(article("a", "Machine title", "machine body\n", "fp1")); err != nil {
		t.Fatal(err)
	}
	// Operator approves: edits status and prose by hand.
	raw, _ := store.Get("a")
	edited := strings.Replace(string(raw), "status: draft", "status: approved", 1)
	edited = strings.Replace(edited, "machine body", "human prose, hard won", 1)
	if err := os.WriteFile(dir+"/a.md", []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(article("a", "Machine retitle", "machine body v2\n", "fp2")); err != nil {
		t.Fatal(err)
	}
	raw, _ = store.Get("a")
	content := string(raw)
	if !strings.Contains(content, "human prose, hard won") || strings.Contains(content, "machine body v2") {
		t.Fatalf("approved body was touched: %q", content)
	}
	if !strings.Contains(content, "Machine title") || strings.Contains(content, "Machine retitle") {
		t.Fatalf("approved title was touched: %q", content)
	}
	articles, _ := store.List()
	if articles[0].Status != "approved" || len(articles[0].Occurrences) != 2 || len(articles[0].Symptom.Fingerprints) != 2 {
		t.Fatalf("approved metadata not merged: %+v", articles[0])
	}
}

func TestGetRejectsPathEscapes(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"../etc/passwd", "a/b", ".."} {
		if _, err := store.Get(slug); !os.IsNotExist(err) {
			t.Fatalf("%q: expected not-exist, got %v", slug, err)
		}
	}
}
