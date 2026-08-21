package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/benemon/shackleton/internal/kb"
	"github.com/benemon/shackleton/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewMCP projects the ask-and-read service operations as an MCP server over
// streamable HTTP, behind the same bearer token as the REST API. Approval
// surfaces are deliberately absent: an external agent deciding approvals
// from inside the human gate would break the trust model, so callers get
// answers, never levers.
func NewMCP(s *Service, token string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "shackleton", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "investigate",
		Description: "Start an investigation of a question about the estate. Returns immediately with the investigation id; investigations take minutes, so follow up with wait_for_verdict or get_investigation."},
		s.mcpInvestigate)
	mcp.AddTool(server, &mcp.Tool{Name: "get_investigation",
		Description: "Get an investigation's current state: status, answer, and structured verdict once completed."},
		s.mcpGetInvestigation)
	mcp.AddTool(server, &mcp.Tool{Name: "wait_for_verdict",
		Description: "Block until an investigation completes or the timeout elapses, then return its state. timeout_seconds defaults to 120 and is capped at 600."},
		s.mcpWaitForVerdict)
	mcp.AddTool(server, &mcp.Tool{Name: "search_kb",
		Description: "Search the knowledge base of resolution articles by substring across slug, title, and symptom; an empty query lists everything."},
		s.mcpSearchKB)
	mcp.AddTool(server, &mcp.Tool{Name: "read_kb",
		Description: "Read a knowledge-base article as markdown by slug."},
		s.mcpReadKB)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return bearerAuth(handler, token)
}

type investigateInput struct {
	Question string `json:"question" jsonschema:"the question to investigate"`
}

type investigationIDInput struct {
	ID string `json:"id" jsonschema:"the investigation id"`
}

type waitForVerdictInput struct {
	ID             string `json:"id" jsonschema:"the investigation id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"seconds to wait before giving up (default 120, max 600)"`
}

type searchKBInput struct {
	Query string `json:"query,omitempty" jsonschema:"substring matched against slug, title, and symptom; empty lists all articles"`
}

type readKBInput struct {
	Slug string `json:"slug" jsonschema:"the article slug"`
}

type kbArticleOutput struct {
	Markdown string `json:"markdown"`
}

type kbSearchOutput struct {
	Articles []kb.FrontMatter `json:"articles"`
}

func (s *Service) mcpInvestigate(ctx context.Context, _ *mcp.CallToolRequest, input investigateInput) (*mcp.CallToolResult, store.Summary, error) {
	if strings.TrimSpace(input.Question) == "" {
		return nil, store.Summary{}, errors.New("question is required")
	}
	summary, err := s.CreateInvestigation(ctx, input.Question, "mcp")
	return nil, summary, err
}

func (s *Service) mcpGetInvestigation(_ context.Context, _ *mcp.CallToolRequest, input investigationIDInput) (*mcp.CallToolResult, store.Summary, error) {
	summary, _, err := s.GetInvestigation(input.ID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, store.Summary{}, fmt.Errorf("investigation %s not found", input.ID)
		}
		return nil, store.Summary{}, err
	}
	return nil, summary, nil
}

func (s *Service) mcpWaitForVerdict(ctx context.Context, request *mcp.CallToolRequest, input waitForVerdictInput) (*mcp.CallToolResult, store.Summary, error) {
	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if timeout > 600*time.Second {
		timeout = 600 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	for {
		result, summary, err := s.mcpGetInvestigation(ctx, request, investigationIDInput{ID: input.ID})
		if err != nil || summary.Status != "running" {
			return result, summary, err
		}
		select {
		case <-ctx.Done():
			return nil, store.Summary{}, ctx.Err()
		case <-deadline.C:
			return nil, summary, nil
		case <-poll.C:
		}
	}
}

func (s *Service) mcpSearchKB(_ context.Context, _ *mcp.CallToolRequest, input searchKBInput) (*mcp.CallToolResult, kbSearchOutput, error) {
	articles, err := s.KBList()
	if err != nil {
		return nil, kbSearchOutput{}, err
	}
	query := strings.ToLower(strings.TrimSpace(input.Query))
	matched := make([]kb.FrontMatter, 0, len(articles))
	for _, article := range articles {
		haystack := strings.ToLower(strings.Join([]string{
			article.Slug, article.Title, article.Symptom.Trigger, article.Symptom.Alertname, article.Symptom.Sweep,
		}, "\n"))
		if query == "" || strings.Contains(haystack, query) {
			matched = append(matched, article)
		}
	}
	return nil, kbSearchOutput{Articles: matched}, nil
}

func (s *Service) mcpReadKB(_ context.Context, _ *mcp.CallToolRequest, input readKBInput) (*mcp.CallToolResult, kbArticleOutput, error) {
	raw, err := s.KBGet(input.Slug)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, kbArticleOutput{}, fmt.Errorf("article %s not found", input.Slug)
		}
		return nil, kbArticleOutput{}, err
	}
	return nil, kbArticleOutput{Markdown: string(raw)}, nil
}
