package main

// wt comments/comment/resolve: argument parsing, the frozen --json golden
// output, and an end-to-end round trip against a fake daemon on a real Unix
// socket (unixSocketServer/shortSocketDir come from integration_test.go, same
// package).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- parseCommentArgs ----

func TestParseCommentArgsPlainBody(t *testing.T) {
	side, author, body := parseCommentArgs([]string{"this", "is", "the", "body"})
	if side != "" || author != "" || body != "this is the body" {
		t.Errorf("got (%q, %q, %q), want (\"\", \"\", \"this is the body\")", side, author, body)
	}
}

func TestParseCommentArgsOldFlag(t *testing.T) {
	side, _, body := parseCommentArgs([]string{"--old", "was", "this", "deleted?"})
	if side != "old" || body != "was this deleted?" {
		t.Errorf("got side=%q body=%q, want old / \"was this deleted?\"", side, body)
	}
}

func TestParseCommentArgsAuthorFlag(t *testing.T) {
	_, author, body := parseCommentArgs([]string{"--author", "naveen", "nice", "catch"})
	if author != "naveen" || body != "nice catch" {
		t.Errorf("got author=%q body=%q, want naveen / \"nice catch\"", author, body)
	}
}

func TestParseCommentArgsBothFlagsAnyOrder(t *testing.T) {
	side, author, body := parseCommentArgs([]string{"--author", "naveen", "--old", "hmm"})
	if side != "old" || author != "naveen" || body != "hmm" {
		t.Errorf("got (%q, %q, %q), want (old, naveen, hmm)", side, author, body)
	}
}

func TestParseCommentArgsNoBodyReturnsEmpty(t *testing.T) {
	_, _, body := parseCommentArgs([]string{"--old"})
	if body != "" {
		t.Errorf("body = %q, want empty (caller treats this as a usage error)", body)
	}
}

func TestParseCommentArgsDanglingAuthorFlagReturnsEmptyBody(t *testing.T) {
	_, author, body := parseCommentArgs([]string{"--author"})
	if body != "" || author != "" {
		t.Errorf("got author=%q body=%q, want both empty (caller treats this as a usage error)", author, body)
	}
}

// ---- renderComments / the frozen --json golden ----

// TestCommentsJSONGoldenMatchesFrozenSchema pins P4-design.md §1.5's example
// verbatim: the same commentsPayload JSON the design's agent-integration
// contract shows, decoded then re-emitted via json.MarshalIndent exactly as
// c.comments(id, true, ...) does. This is the external contract every agent
// integration parses — it must stay byte-stable.
func TestCommentsJSONGoldenMatchesFrozenSchema(t *testing.T) {
	at, err := time.Parse(time.RFC3339, "2026-07-13T10:11:12Z")
	if err != nil {
		t.Fatal(err)
	}
	payload := model.CommentsPayload{
		WorktreeID: "api-server-auth-refactor",
		Path:       "/Users/me/code/api-server-wt/auth-refactor",
		Branch:     "auth-refactor",
		Base:       "main",
		Comments: []model.CommentView{
			{
				Comment: model.Comment{
					ID:         "c-1a2b3c4d5e6f7081",
					WorktreeID: "api-server-auth-refactor",
					File:       "internal/auth/token.go",
					Line:       19,
					Side:       "new",
					Body:       "widen to 30m only for refresh tokens, not access tokens",
					Author:     "naveen",
					State:      "open",
					FileHash:   "9f2c…",
					At:         at,
				},
				Stale:    false,
				Orphaned: false,
			},
		},
	}

	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	want := `{
  "worktreeId": "api-server-auth-refactor",
  "path": "/Users/me/code/api-server-wt/auth-refactor",
  "branch": "auth-refactor",
  "base": "main",
  "comments": [
    {
      "id": "c-1a2b3c4d5e6f7081",
      "worktreeId": "api-server-auth-refactor",
      "file": "internal/auth/token.go",
      "line": 19,
      "side": "new",
      "body": "widen to 30m only for refresh tokens, not access tokens",
      "author": "naveen",
      "state": "open",
      "fileHash": "9f2c…",
      "at": "2026-07-13T10:11:12Z",
      "stale": false,
      "orphaned": false
    }
  ]
}`
	if string(got) != want {
		t.Errorf("MarshalIndent output diverges from P4-design.md §1.5's frozen schema:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestCommentsJSONGoldenEmptyListEncodesAsEmptyArrayNotNull pins that an
// empty comments list must serialise as [], never null — a nil-slice
// footgun that would otherwise make agent-side JSON parsers choke on
// `.comments.map(...)`.
func TestCommentsJSONGoldenEmptyListEncodesAsEmptyArrayNotNull(t *testing.T) {
	payload := model.CommentsPayload{WorktreeID: "wt-1", Comments: []model.CommentView{}}
	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"comments": []`)) {
		t.Errorf("got %s, want \"comments\": [] for an empty (non-nil) slice", got)
	}
}

// ---- end-to-end over a real Unix socket ----

// TestWtCommentCommentsResolveRoundTrip runs the actual wt binary three times
// (comment, comments --json, resolve, comments --json again) against a fake
// wtd on a real Unix socket, proving the full CLI surface, not just its
// in-process pieces.
func TestWtCommentCommentsResolveRoundTrip(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")

	const wtID = "api-server-feature"
	var stored *model.Comment

	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": model.ProtocolVersion, "version": "x", "goVersion": "go1.24"})
	})
	mux.HandleFunc("/api/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req struct {
				ID, File, Side, Body, Author string
				Line                         int
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			c := model.Comment{
				ID: "c-abc123", WorktreeID: req.ID, File: req.File, Line: req.Line,
				Side: req.Side, Body: req.Body, Author: req.Author, State: "open",
			}
			if c.Side == "" {
				c.Side = "new"
			}
			stored = &c
			_ = json.NewEncoder(w).Encode(c)
		case http.MethodGet:
			payload := model.CommentsPayload{WorktreeID: wtID, Path: "/repo/" + wtID, Branch: "feature", Base: "main"}
			if stored != nil {
				want := r.URL.Query().Get("state")
				if want == "all" || (want == "" && stored.State == "open") || want == stored.State {
					payload.Comments = []model.CommentView{{Comment: *stored}}
				} else {
					payload.Comments = []model.CommentView{}
				}
			} else {
				payload.Comments = []model.CommentView{}
			}
			_ = json.NewEncoder(w).Encode(payload)
		}
	})
	mux.HandleFunc("/api/comments/resolve", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CommentID string `json:"commentId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if stored == nil || stored.ID != req.CommentID {
			http.Error(w, "comment not found", http.StatusNotFound)
			return
		}
		stored.State = "resolved"
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	ts := unixSocketServer(t, sockPath, mux)
	defer ts.Close()

	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	if out, errOut, err := run("comment", wtID, "internal/auth/token.go", "19", "--author", "naveen", "widen", "to", "30m"); err != nil {
		t.Fatalf("wt comment failed: %v\nstdout=%s\nstderr=%s", err, out, errOut)
	} else if !bytes.Contains([]byte(out), []byte("c-abc123")) {
		t.Errorf("wt comment stdout = %q, want it to mention the created comment id", out)
	}
	if stored == nil || stored.Body != "widen to 30m" || stored.Author != "naveen" {
		t.Fatalf("daemon did not receive the expected comment: %+v", stored)
	}

	out, errOut, err := run("comments", wtID, "--json")
	if err != nil {
		t.Fatalf("wt comments --json failed: %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}
	var payload model.CommentsPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("wt comments --json did not print valid JSON: %v\noutput:\n%s", err, out)
	}
	if len(payload.Comments) != 1 || payload.Comments[0].ID != "c-abc123" || payload.Comments[0].State != "open" {
		t.Errorf("wt comments --json = %+v, want the one open comment just created", payload)
	}

	if out, errOut, err := run("resolve", wtID, "c-abc123"); err != nil {
		t.Fatalf("wt resolve failed: %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}
	if stored.State != "resolved" {
		t.Fatalf("daemon-side state = %q, want resolved", stored.State)
	}

	out, _, err = run("comments", wtID, "--json")
	if err != nil {
		t.Fatalf("wt comments --json (after resolve) failed: %v", err)
	}
	var afterResolve model.CommentsPayload
	if err := json.Unmarshal([]byte(out), &afterResolve); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(afterResolve.Comments) != 0 {
		t.Errorf("default (open-only) list after resolve = %+v, want empty", afterResolve.Comments)
	}

	out, _, err = run("comments", wtID, "--json", "--all")
	if err != nil {
		t.Fatalf("wt comments --json --all failed: %v", err)
	}
	var all model.CommentsPayload
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(all.Comments) != 1 || all.Comments[0].State != "resolved" {
		t.Errorf("--all list after resolve = %+v, want the one resolved comment", all)
	}
}
