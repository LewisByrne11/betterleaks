package scm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSanitizeOutput(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		token   string
		notWant []string // none of these substrings should appear in output
		want    string   // optional: exact expected output
	}{
		{
			name:    "redacts raw token",
			text:    "fatal: could not auth using ghp_secrettoken",
			token:   "ghp_secrettoken",
			notWant: []string{"ghp_secrettoken"},
		},
		{
			name:  "redacts URL userinfo even if token absent",
			text:  "fatal: cloning https://x-access-token:ghp_secret@github.com/foo/bar.git failed",
			token: "",
			want:  "fatal: cloning https://***@github.com/foo/bar.git failed",
		},
		{
			name:    "redacts URL userinfo when token also matches",
			text:    "cloning https://x-access-token:ghp_secret@github.com/foo/bar.git",
			token:   "ghp_secret",
			notWant: []string{"ghp_secret", "x-access-token:"},
		},
		{
			name:  "empty text",
			text:  "",
			token: "ghp_secret",
			want:  "",
		},
		{
			name:  "no token, no userinfo",
			text:  "fatal: not a git repository",
			token: "",
			want:  "fatal: not a git repository",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeOutput(tc.text, tc.token)
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("SanitizeOutput leaked %q in %q", bad, got)
				}
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("SanitizeOutput() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCloneTransportBoundary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected credentials on anonymous request")
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	var opts CloneOptions
	for _, remote := range []string{"ssh://git@example.test/repo", "file:///tmp/repo", "git@example.test:repo", server.URL + "/repo?token=secret", server.URL + "/repo?", "http://user:secret@" + strings.TrimPrefix(server.URL, "http://") + "/repo"} {
		err := CloneAuthed(t.Context(), remote, "", filepath.Join(t.TempDir(), "repo"), opts)
		if err == nil || !strings.Contains(err.Error(), "HTTP(S) URL") {
			t.Fatalf("unexpected URL validation result for %q: %v", remote, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("URL credentials leaked: %v", err)
		}
	}
	if err := CloneAuthed(t.Context(), server.URL+"/repo", "secret", filepath.Join(t.TempDir(), "repo"), opts); err == nil || !strings.Contains(err.Error(), "require HTTPS") {
		t.Fatalf("expected insecure token rejection, got %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("rejected clone made a network request")
	}
	if err := CloneAuthed(t.Context(), server.URL+"/repo", "", filepath.Join(t.TempDir(), "repo"), opts); err == nil {
		t.Fatal("expected missing repository error")
	}
	if requests.Load() == 0 {
		t.Fatal("clone did not use HTTP with an empty PATH")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := CloneAuthed(ctx, server.URL+"/repo", "", filepath.Join(t.TempDir(), "repo"), opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
