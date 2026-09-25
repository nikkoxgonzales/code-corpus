// Package rerank judges search candidates with a System One model (Jev), reached
// through OpenRouter or TypeSafe's own API. Both speak the same /v1/systemone
// protocol; only base URL, key and model name differ.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BatchSize is the number of candidates judged per request.
const BatchSize = 30

type Item struct {
	ID     string // caller's key
	Header string // e.g. "repo:path:10-40 func Foo"
	Text   string // code excerpt
}

type Result struct {
	Scores   map[string]float64 // ID -> probability the item answers the query
	Provider string
	Model    string
	CostUSD  float64
	Tokens   int
}

type Config struct {
	Provider string // "openrouter" | "typesafe"
	BaseURL  string
	Model    string
	key      string
	Timeout  time.Duration
}

// Describe is a one-line, key-free summary for status output.
func (c *Config) Describe() string {
	return fmt.Sprintf("%s (%s, timeout %s)", c.Provider, c.Model, c.Timeout)
}

// FromEnv picks a provider. OpenRouter wins when both keys are present.
//
//	CORPUS_RERANK=off|auto|openrouter|typesafe   (default auto)
//	OPENROUTER_API_KEY / TYPESAFE_API_KEY
//	CORPUS_RERANK_MODEL, CORPUS_RERANK_BASE_URL, CORPUS_RERANK_TIMEOUT_MS
//
// It returns nil and a reason when reranking is unavailable.
func FromEnv() (*Config, string) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("CORPUS_RERANK")))
	if mode == "off" || mode == "0" || mode == "false" {
		return nil, "disabled by CORPUS_RERANK=off"
	}
	or, ts := os.Getenv("OPENROUTER_API_KEY"), os.Getenv("TYPESAFE_API_KEY")
	var c *Config
	switch {
	case (mode == "" || mode == "auto" || mode == "openrouter") && or != "":
		c = &Config{Provider: "openrouter", BaseURL: "https://openrouter.ai/api", Model: "typesafe/jev-1.13", key: or}
	case (mode == "" || mode == "auto" || mode == "typesafe") && ts != "":
		c = &Config{Provider: "typesafe", BaseURL: "https://api.typesafe.ai", Model: "jev-latest", key: ts}
	default:
		return nil, "no OPENROUTER_API_KEY or TYPESAFE_API_KEY set"
	}
	if m := os.Getenv("CORPUS_RERANK_MODEL"); m != "" {
		c.Model = m
	}
	if b := os.Getenv("CORPUS_RERANK_BASE_URL"); b != "" {
		c.BaseURL = strings.TrimSuffix(b, "/")
	}
	c.Timeout = 8 * time.Second
	if ms, err := strconv.Atoi(os.Getenv("CORPUS_RERANK_TIMEOUT_MS")); err == nil && ms > 0 {
		c.Timeout = time.Duration(ms) * time.Millisecond
	}
	return c, ""
}

const instructions = `Does this excerpt directly implement, define, or answer the code search query in the state? ` +
	`Answer yes only if a developer asking that query would want to read this exact excerpt: ` +
	`not if it merely mentions related words, only calls the relevant code, or is an unrelated test.`

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type request struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]question `json:"questions"`
}

type response struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Noul *float64 `json:"noul"`
	} `json:"answers"`
	Usage struct {
		InputTokens int      `json:"input_tokens"`
		Cost        *float64 `json:"cost"`
	} `json:"usage"`
	Error any `json:"error"`
}

// Judge scores every item against the query. Batches run in parallel.
// A failed batch fails the whole call so callers can fall back cleanly.
func (c *Config) Judge(ctx context.Context, query string, items []Item) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	res := &Result{Scores: map[string]float64{}, Provider: c.Provider, Model: c.Model}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for lo := 0; lo < len(items); lo += BatchSize {
		batch := items[lo:min(lo+BatchSize, len(items))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.call(ctx, query, batch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for k, v := range r.scores {
				res.Scores[k] = v
			}
			res.CostUSD += r.cost
			res.Tokens += r.tokens
			if r.model != "" {
				res.Model = r.model
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return res, nil
}

type batchResult struct {
	scores map[string]float64
	cost   float64
	tokens int
	model  string
}

func (c *Config) call(ctx context.Context, query string, items []Item) (*batchResult, error) {
	req := request{
		Model:     c.Model,
		State:     "Code search query: " + query,
		Questions: map[string]question{},
	}
	keys := map[string]string{}
	for i, it := range items {
		k := "c" + strconv.Itoa(i)
		keys[k] = it.ID
		req.Questions[k] = question{Type: "noul", Instructions: instructions + "\n\n" + it.Header + "\n```\n" + it.Text + "\n```"}
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+c.key)
	hr.Header.Set("Content-Type", "application/json")
	if c.Provider == "openrouter" {
		hr.Header.Set("X-Title", "code-corpus")
		hr.Header.Set("HTTP-Referer", "https://github.com/nikkoxgonzales/code-corpus")
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timeout after %s", c.Timeout)
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("%s HTTP %d: %s", c.Provider, resp.StatusCode, msg)
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: bad response: %v", c.Provider, err)
	}
	out := &batchResult{scores: map[string]float64{}, tokens: r.Usage.InputTokens, model: r.Model}
	for k, a := range r.Answers {
		if a.Noul != nil {
			out.scores[keys[k]] = *a.Noul
		}
	}
	if r.Usage.Cost != nil {
		out.cost = *r.Usage.Cost
	} else {
		out.cost = float64(r.Usage.InputTokens) * 0.042 / 1e6
	}
	return out, nil
}
