package gogit

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func patchTestKey(i int) patchKey {
	return patchKey{to: plumbing.NewHash(fmt.Sprintf("%040x", i+1))}
}

func TestPatchCacheBoundsAndOwnership(t *testing.T) {
	c := &patchCache{}
	first := []textAddition{{raw: strings.Repeat("a", 128<<10), line: 7}}
	c.put(patchTestKey(0), first)
	borrowed, ok := c.get(patchTestKey(0))
	require.True(t, ok)
	for i := 1; i < 300; i++ {
		c.put(patchTestKey(i), []textAddition{{raw: strings.Repeat("b", 128<<10), line: i}})
		require.LessOrEqual(t, c.bytes, patchCacheBudget)
	}
	_, ok = c.get(patchTestKey(0))
	require.False(t, ok)
	require.Equal(t, first, borrowed, "eviction must not mutate an in-flight patch")
	c.put(patchTestKey(300), []textAddition{{raw: strings.Repeat("x", maxCachedPatch)}})
	_, ok = c.get(patchTestKey(300))
	require.False(t, ok)
	c.put(patchTestKey(301), nil)
	value, ok := c.get(patchTestKey(301))
	require.True(t, ok, "a deletion-only text change is a valid empty result")
	require.Empty(t, value)
}

func TestPatchCacheConcurrent(t *testing.T) {
	c := &patchCache{}
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				key := patchTestKey(i + worker*1000)
				c.put(key, []textAddition{{raw: "TOKEN\n", line: i}})
				if value, ok := c.get(key); ok && (len(value) != 1 || value[0].line != i || value[0].raw != "TOKEN\n") {
					t.Errorf("patch content changed for %s", key.to)
				}
			}
		})
	}
	wg.Wait()
}

func TestCachedPatchCallbacks(t *testing.T) {
	objects := &objectCache{cache.NewObjectLRU(1 << 20)}
	from := &storedObject{id: plumbing.NewHash(strings.Repeat("1", 40)), kind: plumbing.BlobObject, raw: "first\nold\nlast\n"}
	to := &storedObject{id: plumbing.NewHash(strings.Repeat("2", 40)), kind: plumbing.BlobObject, raw: "first\nTOKEN\nlast\n"}
	objects.Put(from)
	objects.Put(to)
	w := worker{source: &Source{}, store: &objectStorage{ctx: t.Context(), objects: objects}, patches: &patchCache{}, maxBlob: 1 << 20}
	change := &object.Change{
		From: object.ChangeEntry{Name: "old.txt", TreeEntry: object.TreeEntry{Hash: from.id, Mode: filemode.Regular}},
		To:   object.ChangeEntry{Name: "new.txt", TreeEntry: object.TreeEntry{Hash: to.id, Mode: filemode.Regular}},
	}
	key := patchKey{from.id, to.id}
	failed := errors.New("callback failed")
	emitError := func(string, int) error { return failed }
	require.ErrorIs(t, w.change(t.Context(), change, nil, emitError, nil), failed)
	_, ok := w.patches.get(key)
	require.False(t, ok, "failed emission must not populate the cache")
	var got []sources.Fragment
	for _, commit := range []string{"first-commit", "second-commit"} {
		attrs := map[string]string{sources.AttrGitSHA: commit, sources.AttrPath: commit + ".txt"}
		emit := func(raw string, line int) error {
			got = append(got, sources.Fragment{Raw: raw, StartLine: line, Attributes: attrs})
			return nil
		}
		require.NoError(t, w.change(t.Context(), change, attrs, emit, nil))
		// The second invocation must use only cached additions and the new
		// callback, without reading an object or retaining the old attributes.
		w.store = nil
	}
	require.Len(t, got, 2)
	for i, commit := range []string{"first-commit", "second-commit"} {
		require.Equal(t, "TOKEN\n", got[i].Raw)
		require.Equal(t, 2, got[i].StartLine)
		require.Equal(t, commit, got[i].Attr(sources.AttrGitSHA))
		require.Equal(t, commit+".txt", got[i].Attr(sources.AttrPath))
	}
	require.ErrorIs(t, w.change(t.Context(), change, nil, emitError, nil), failed)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, w.change(ctx, change, nil, emitError, nil), context.Canceled)
}

func TestPatchCachePreservesBinaryArchiveMembers(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	member, err := writer.Create("secret.bin")
	require.NoError(t, err)
	content := "\x00TOKEN_BINARY_MEMBER\x00\n"
	_, err = member.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	objects := &objectCache{cache.NewObjectLRU(1 << 20)}
	to := &storedObject{id: plumbing.NewHash(strings.Repeat("2", 40)), kind: plumbing.BlobObject, raw: archive.String()}
	objects.Put(to)
	w := worker{source: &Source{MaxArchiveDepth: 1}, store: &objectStorage{ctx: t.Context(), objects: objects}, patches: &patchCache{}, maxBlob: 1 << 20}
	change := &object.Change{
		From: object.ChangeEntry{Name: "archive.zip", TreeEntry: object.TreeEntry{Hash: plumbing.NewHash(strings.Repeat("1", 40)), Mode: filemode.Regular}},
		To:   object.ChangeEntry{Name: "archive.zip", TreeEntry: object.TreeEntry{Hash: to.id, Mode: filemode.Regular}},
	}
	for _, commit := range []string{"first-commit", "second-commit"} {
		var got []sources.Fragment
		attrs := map[string]string{sources.AttrGitSHA: commit, sources.AttrPath: "archive.zip"}
		yield := func(f sources.Fragment, err error) error {
			require.NoError(t, err)
			got = append(got, f)
			return nil
		}
		require.NoError(t, w.change(t.Context(), change, attrs, nil, yield))
		require.Len(t, got, 1)
		require.Equal(t, content, got[0].Raw)
		require.Equal(t, commit, got[0].Attr(sources.AttrGitSHA))
		require.Equal(t, "archive.zip!secret.bin", got[0].Attr(sources.AttrPath))
	}
	require.Empty(t, w.patches.entries, "archive extraction must run for every commit")
}
