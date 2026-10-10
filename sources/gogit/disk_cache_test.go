package gogit

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func diskTestObject(i int) *storedObject {
	return &storedObject{id: plumbing.NewHash(fmt.Sprintf("%040x", i+1)), kind: plumbing.BlobObject, raw: strings.Repeat(string(rune('a'+i%26)), 1<<20)}
}

func TestDiskObjectCache(t *testing.T) {
	c := &diskObjectCache{segmentSize: 1 << 20}
	defer c.close()
	first := diskTestObject(0)
	c.put(first)
	info, err := c.file.Stat()
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
	read, ok := c.get(first.id)
	require.True(t, ok)
	require.Equal(t, first, read)
	for i := 1; i < 20; i++ {
		c.put(diskTestObject(i))
	}
	_, ok = c.get(first.id)
	require.False(t, ok)
	require.Equal(t, first.raw, read.raw, "eviction must not overwrite borrowed strings")
	last := diskTestObject(19)
	read, ok = c.get(last.id)
	require.True(t, ok)
	require.Equal(t, last, read)
	info, err = c.file.Stat()
	require.NoError(t, err)
	require.LessOrEqual(t, info.Size(), int64(8<<20))
	name := c.file.Name()
	c.close()
	_, err = os.Stat(name)
	require.True(t, os.IsNotExist(err))
}

func TestDiskObjectCacheConcurrent(t *testing.T) {
	c := &diskObjectCache{segmentSize: 1 << 20}
	defer c.close()
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 10 {
				o := diskTestObject(worker*10 + i)
				c.put(o)
				if read, ok := c.get(o.id); ok && *read != *o {
					t.Errorf("cache returned different content for %s", o.id)
				}
			}
		})
	}
	wg.Wait()
}

func TestDiskObjectCacheUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(path, nil, 0600))
	t.Setenv("TMPDIR", path)
	t.Setenv("TMP", path)
	t.Setenv("TEMP", path)
	c := &diskObjectCache{segmentSize: 1 << 20}
	defer c.close()
	o := diskTestObject(0)
	c.put(o)
	require.True(t, c.failed)
	_, ok := c.get(o.id)
	require.False(t, ok)
}
