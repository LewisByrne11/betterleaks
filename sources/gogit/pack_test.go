package gogit

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/stretchr/testify/require"
)

type packBytes struct{ *bytes.Reader }

func (packBytes) Close() error { return nil }

func TestPackedObjectBounds(t *testing.T) {
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	_, err := z.Write([]byte("abc"))
	require.NoError(t, err)
	require.NoError(t, z.Close())
	good := append([]byte{0x33}, compressed.Bytes()...)
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"valid", good, true},
		{"invalid-type", []byte{0x50}, false},
		{"truncated-header", []byte{0xb0}, false},
		{"overflow", append([]byte{0xb0}, bytes.Repeat([]byte{0xff}, 10)...), false},
		{"invalid-zlib", []byte{0x33, 0, 0}, false},
		{"truncated-zlib", good[:len(good)-1], false},
		{"overrun", append([]byte{0x32}, compressed.Bytes()...), false},
		{"short-result", append([]byte{0x34}, compressed.Bytes()...), false},
		{"zero-delta-offset", []byte{0x60, 0}, false},
		{"delta-offset-before-header", []byte{0x60, 1}, false},
		{"truncated-reference", []byte{0x70, 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(make([]byte, 12), tc.data...)
			p := &objectPack{file: packBytes{bytes.NewReader(data)}, end: int64(len(data)), hashSize: 20}
			s := &objectStorage{ctx: t.Context(), limit: 1024}
			kind, _, _, raw, err := s.inflate(p, 12)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, plumbing.BlobObject, kind)
			require.Equal(t, "abc", raw)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			s.ctx = ctx
			_, _, _, _, err = s.inflate(p, 12)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func FuzzPackedObject(f *testing.F) {
	f.Add([]byte{0xb0, 0xff, 0xff})
	f.Add([]byte{0x60, 0})
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 65536 {
			t.Skip()
		}
		data := append(make([]byte, 12), input...)
		p := &objectPack{file: packBytes{bytes.NewReader(data)}, end: int64(len(data)), hashSize: 20}
		s := &objectStorage{ctx: t.Context(), limit: 65536}
		_, _, _, raw, err := s.inflate(p, 12)
		if err == nil {
			require.LessOrEqual(t, len(raw), 65536)
		}
	})
}

func TestPackedObjects(t *testing.T) {
	for _, objectFormat := range []format.ObjectFormat{format.SHA1, format.SHA256} {
		for _, offsets := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/offsets=%v", objectFormat, offsets), func(t *testing.T) {
				repo := t.TempDir()
				gitFixture(t, repo, "init", "-q", "--object-format="+string(objectFormat))
				gitFixture(t, repo, "config", "user.name", "Pack Test")
				gitFixture(t, repo, "config", "user.email", "pack@example.test")
				for revision := range 20 {
					var content strings.Builder
					for line := range 1000 {
						value := 0
						if line%37 == 0 {
							value = revision
						}
						fmt.Fprintf(&content, "line %04d with some shared content %d\n", line, value)
					}
					require.NoError(t, os.WriteFile(filepath.Join(repo, "data"), []byte(content.String()), 0600))
					gitFixture(t, repo, "add", "data")
					gitFixture(t, repo, "commit", "-qm", strconv.Itoa(revision))
				}
				args := []string{"-c", "pack.writeReverseIndex=true", "pack-objects", "--all", "--window=20", "--depth=20", ".git/objects/pack/pack"}
				if offsets {
					args = append(args, "--delta-base-offset")
				}
				gitFixture(t, repo, args...)
				r, objects, close, err := (&Source{RepoPath: repo, CacheMiB: 1}).open(t.Context())
				require.NoError(t, err)
				defer close()
				store := r.Storer.(*objectStorage)
				require.Len(t, store.packs, 1)
				p := store.packs[0]
				iter, err := p.index.Entries()
				require.NoError(t, err)
				defer iter.Close()
				count, deltas := 0, 0
				for {
					e, err := iter.Next()
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					kind, _, _, _, err := store.inflate(p, int64(e.Offset))
					require.NoError(t, err)
					if kind.IsDelta() {
						deltas++
						want := plumbing.REFDeltaObject
						if offsets {
							want = plumbing.OFSDeltaObject
						}
						require.Equal(t, want, kind)
					}
					objects.Clear() // Exercise reconstruction, not an earlier cache hit.
					o, err := store.read(e.Hash, 0)
					require.NoError(t, err)
					h := plumbing.NewHasher(objectFormat, o.Type(), o.Size())
					h.Write([]byte(o.raw))
					require.Equal(t, e.Hash, h.Sum())
					count++
				}
				require.Equal(t, 60, count)
				require.Greater(t, deltas, 10)
			})
		}
	}
}

func TestPackWithoutReverseIndex(t *testing.T) {
	repo := newHistory(t)
	gitFixture(t, repo, "-c", "pack.writeReverseIndex=false", "repack", "-ad")
	r, _, close, err := (&Source{RepoPath: repo}).open(t.Context())
	require.NoError(t, err)
	require.Empty(t, r.Storer.(*objectStorage).packs)
	close()
	t.Setenv("PATH", t.TempDir())
	fs := collect(t, &Source{RepoPath: repo})
	require.Equal(t, "file.txt:2", textOccurrences(fs)["TOKEN_CHANGED\n"])
}
