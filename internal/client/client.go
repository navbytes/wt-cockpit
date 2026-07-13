// Package client is the one shared thin HTTP client every wt-cockpit
// frontend — the CLI (cmd/wt) and the TUI (internal/tui) — uses to talk to
// wtd. It holds no git logic and no engine imports: it only ever speaks
// JSON+HTTP and SSE over wtd's Unix socket (docs/02-stack-decision.md). This
// package, plus its method set below, is frozen as of wt-cockpit v0.3 WP1:
// WP2/WP3 may add fields to their own new types but must not change these
// signatures.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// Client is the shared connection to wtd. Construct with New.
type Client struct {
	http *http.Client // REST calls: bounded per-request timeout
	sse  *http.Client // /api/events: no overall timeout — it's a long-lived stream
	base string
}

// New dials socket lazily (the first request triggers the actual connect;
// construction never blocks or errors). Every frontend resolves socket the
// same way: SocketFromEnv, below.
func New(socket string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	transport := &http.Transport{DialContext: dial}
	return &Client{
		base: "http://unix",
		http: &http.Client{Timeout: 10 * time.Second, Transport: transport},
		// No Timeout here: net/http's Client.Timeout bounds the whole
		// round trip *including reading the body*, which would silently
		// sever a long-lived SSE stream after 10s. Sharing the Transport
		// keeps the same dial/pooling behaviour for both.
		sse: &http.Client{Transport: transport},
	}
}

// SocketFromEnv resolves the daemon socket path the same way every wt-cockpit
// frontend always has: WTD_SOCKET if set, else ~/.wtcockpit/wtd.sock.
func SocketFromEnv() string {
	home, _ := os.UserHomeDir()
	socket := filepath.Join(home, ".wtcockpit", "wtd.sock")
	if s := os.Getenv("WTD_SOCKET"); s != "" {
		socket = s
	}
	return socket
}

// UnreachableError means the request never got a response from wtd at all
// (connection refused, no such socket, timeout) — as opposed to wtd
// answering with a non-2xx status. Callers distinguish "daemon down" (worth
// retrying) from "daemon answered but refused/errored" (usually not).
type UnreachableError struct{ Err error }

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("cannot reach wtd (is it running?): %v", e.Err)
}
func (e *UnreachableError) Unwrap() error { return e.Err }

// ConflictError is SetReviewed's 409: the file changed since the caller's
// expected hash was captured. Msg is the daemon's response body verbatim.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// GateError is Approve's 409: a gate (full review / clean tree / clean
// merge) refused the request. Msg is the daemon's response body verbatim.
type GateError struct{ Msg string }

func (e *GateError) Error() string { return e.Msg }

// do executes req against wtd, shaping a transport-level failure into
// *UnreachableError (today's CLI voice: "cannot reach wtd (is it running?)").
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &UnreachableError{Err: err}
	}
	return resp, nil
}

// readError shapes a non-2xx response into an error: the daemon puts the
// human-readable reason in the body (e.g. a refused approve gate), so that's
// the message when present, falling back to the bare HTTP status text when
// the body is empty.
func readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return errors.New(msg)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Version fetches wtd's protocol handshake payload. It does not itself judge
// a mismatch — model.ProtocolVersion comparison and the resulting UI/exit
// behaviour is a frontend decision (the CLI and the TUI react differently).
// A 404 means wtd predates the handshake entirely (pre-v0.2).
func (c *Client) Version(ctx context.Context) (protocol int, version string, err error) {
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/version", nil)
	if reqErr != nil {
		return 0, "", reqErr
	}
	resp, doErr := c.do(req)
	if doErr != nil {
		return 0, "", doErr
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, "", errors.New("wtd does not support the protocol handshake (pre-v0.2); rebuild/restart wtd")
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", readError(resp)
	}
	var v struct {
		Protocol int    `json:"protocol"`
		Version  string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return 0, "", fmt.Errorf("wtd /api/version: %w", err)
	}
	return v.Protocol, v.Version, nil
}

// Worktrees fetches the full worktree radar snapshot.
func (c *Client) Worktrees(ctx context.Context) ([]model.Worktree, error) {
	var wts []model.Worktree
	if err := c.getJSON(ctx, "/api/worktrees", &wts); err != nil {
		return nil, err
	}
	return wts, nil
}

// Diff fetches one worktree's structured diff.
func (c *Client) Diff(ctx context.Context, id string) (model.Diff, error) {
	var d model.Diff
	err := c.getJSON(ctx, "/api/diff?id="+url.QueryEscape(id), &d)
	return d, err
}

// Rules fetches the effective, provenance-tagged rule set for worktree id's
// owning repo (P5-design.md §1.3) — `wt rules`'s data source. The payload
// type lives in internal/guardrail, which — like this package — imports only
// model and stdlib, so pulling it in here doesn't drag the engine (or any
// other daemon-only package) into the thin client.
func (c *Client) Rules(ctx context.Context, id string) (guardrail.Effective, error) {
	var eff guardrail.Effective
	err := c.getJSON(ctx, "/api/rules?id="+url.QueryEscape(id), &eff)
	return eff, err
}

// SetReviewed toggles a file's reviewed state. hash, when non-empty, must
// match the file's current diff hash or the daemon refuses with 409,
// returned here as *ConflictError.
func (c *Client) SetReviewed(ctx context.Context, id, file string, reviewed bool, hash string) error {
	payload, _ := json.Marshal(map[string]any{"id": id, "file": file, "reviewed": reviewed, "hash": hash})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/review", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return &ConflictError{Msg: readError(resp).Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return nil
}

// Approve is the engine's only mutation: merge the worktree's branch into
// its base and remove the worktree, gated on full review + a clean tree. A
// refused gate comes back as 409, returned here as *GateError.
func (c *Client) Approve(ctx context.Context, id string) (model.ApproveResult, error) {
	payload, _ := json.Marshal(map[string]any{"id": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/approve", bytes.NewReader(payload))
	if err != nil {
		return model.ApproveResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return model.ApproveResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return model.ApproveResult{}, &GateError{Msg: readError(resp).Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return model.ApproveResult{}, readError(resp)
	}
	var res model.ApproveResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return model.ApproveResult{}, err
	}
	return res, nil
}

// Refresh forces a full daemon rescan.
func (c *Client) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/refresh", strings.NewReader(""))
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return nil
}

// AddComment creates a comment on file within id's current diff (P4-design.md
// §1.5). line 0 is the file-level convention; side "" lets the daemon default
// to "new"; author "" lets it default to the daemon's OS user. A validation
// refusal (unknown id/file, bad side, empty/oversized/invalid-UTF-8 body)
// comes back as the daemon's body verbatim via the plain error path — unlike
// SetReviewed/Approve there's no single typed sentinel here, since the REST
// table maps several distinct causes to more than one status code.
func (c *Client) AddComment(ctx context.Context, id, file string, line int, side, body, author string) (model.Comment, error) {
	payload, _ := json.Marshal(map[string]any{
		"id": id, "file": file, "line": line, "side": side, "body": body, "author": author,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/comments", bytes.NewReader(payload))
	if err != nil {
		return model.Comment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return model.Comment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return model.Comment{}, readError(resp)
	}
	var out model.Comment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return model.Comment{}, err
	}
	return out, nil
}

// Comments lists comments for id. state selects "open" (the daemon's default
// when empty), "resolved", or "all"; file, when non-empty, filters to that
// exact path. The returned model.CommentsPayload is GET /api/comments's
// frozen response shape — cmd/wt's `wt comments --json` decodes this exact
// type and re-emits it with MarshalIndent, so the API and the CLI can never
// drift apart (P4-design.md §1.5).
func (c *Client) Comments(ctx context.Context, id, state, file string) (model.CommentsPayload, error) {
	q := url.Values{"id": {id}}
	if state != "" {
		q.Set("state", state)
	}
	if file != "" {
		q.Set("file", file)
	}
	var out model.CommentsPayload
	err := c.getJSON(ctx, "/api/comments?"+q.Encode(), &out)
	return out, err
}

// ResolveComment marks a comment resolved — the agent's loop-closer.
func (c *Client) ResolveComment(ctx context.Context, id, commentID string) error {
	return c.postCommentAction(ctx, "/api/comments/resolve", id, commentID)
}

// DeleteComment removes a comment outright.
func (c *Client) DeleteComment(ctx context.Context, id, commentID string) error {
	return c.postCommentAction(ctx, "/api/comments/delete", id, commentID)
}

func (c *Client) postCommentAction(ctx context.Context, path, id, commentID string) error {
	payload, _ := json.Marshal(map[string]any{"id": id, "commentId": commentID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return nil
}
