package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// LLMCall is one row of llm_calls: one call to the LLM provider, or one
// call the server refused or served from the cache.
type LLMCall struct {
	ID            string
	Role          string
	Model         string
	BaseHost      string
	At            time.Time
	LatencyMS     int64
	TokensIn      int64
	TokensOut     int64
	InputHash     string
	PromptVersion string
	Status        string
	// Output is the provider's answer as JSON; nil for none.
	Output json.RawMessage
	Error  string
}

// LLM call statuses.
const (
	LLMOK      = "ok"
	LLMError   = "error"
	LLMTimeout = "timeout"
	LLMInvalid = "invalid"
	LLMCached  = "cached"
)

const llmCols = `id, role, model, base_host, at, latency_ms, tokens_in, tokens_out, input_hash, prompt_version,
	status, output, error`

func scanLLMCall(r scanner) (LLMCall, error) {
	var c LLMCall
	var at string
	var output, errText sql.NullString
	if err := r.Scan(&c.ID, &c.Role, &c.Model, &c.BaseHost, &at, &c.LatencyMS, &c.TokensIn, &c.TokensOut,
		&c.InputHash, &c.PromptVersion, &c.Status, &output, &errText); err != nil {
		return LLMCall{}, err
	}
	c.At, c.Error = parseTime(at), errText.String
	if output.Valid && output.String != "" {
		c.Output = json.RawMessage(output.String)
	}
	return c, nil
}

// AddLLMCall stores c. c.ID and c.At must be set.
func (s *Store) AddLLMCall(ctx context.Context, c LLMCall) error {
	var output any
	if len(c.Output) > 0 {
		output = string(c.Output)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO llm_calls (`+llmCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Role, c.Model, c.BaseHost, fmtTime(c.At), c.LatencyMS, c.TokensIn, c.TokensOut, c.InputHash,
		c.PromptVersion, c.Status, output, nullStr(c.Error))
	return s.bumpIf(err, DomainLLM)
}

// CachedLLMCall returns the newest status ok call with hash at or after
// since, or ErrNotFound.
func (s *Store) CachedLLMCall(ctx context.Context, hash string, since time.Time) (LLMCall, error) {
	c, err := scanLLMCall(s.db.QueryRowContext(ctx, `SELECT `+llmCols+` FROM llm_calls
		WHERE input_hash = ? AND at >= ? AND status = ? ORDER BY at DESC LIMIT 1`, hash, fmtTime(since), LLMOK))
	if errors.Is(err, sql.ErrNoRows) {
		return LLMCall{}, ErrNotFound
	}
	return c, err
}

// LLMTokensSince returns the tokens in and out of the calls at or after
// since.
func (s *Store) LLMTokensSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(tokens_in + tokens_out), 0) FROM llm_calls WHERE at >= ?`,
		fmtTime(since)).Scan(&n)
	return n, err
}

// LLMCalls returns the newest calls first, at most limit.
func (s *Store) LLMCalls(ctx context.Context, limit int) ([]LLMCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+llmCols+` FROM llm_calls ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMCall
	for rows.Next() {
		c, err := scanLLMCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetLLMCall returns the call with id.
func (s *Store) GetLLMCall(ctx context.Context, id string) (LLMCall, error) {
	c, err := scanLLMCall(s.db.QueryRowContext(ctx, `SELECT `+llmCols+` FROM llm_calls WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return LLMCall{}, ErrNotFound
	}
	return c, err
}

// PruneLLMCalls removes calls before before and returns how many.
func (s *Store) PruneLLMCalls(ctx context.Context, before time.Time) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM llm_calls WHERE at < ?`, fmtTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
