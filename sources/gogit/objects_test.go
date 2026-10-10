package gogit

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/stretchr/testify/require"
)

func TestApplyDelta(t *testing.T) {
	for _, tc := range []struct {
		name, base, delta, want string
	}{
		{"copy-and-literal", "abc", "\x03\x04\x90\x03\x01d", "abcd"},
		{"empty-base", "", "\x00\x03\x03abc", "abc"},
		{"empty-result", "abc", "\x03\x00", ""},
		{"offset", "abcdef", "\x06\x03\x91\x02\x03", "cde"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyDelta(t.Context(), tc.base, tc.delta, 1<<20)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	base := strings.Repeat("x", 65536)
	header := binary.AppendUvarint(nil, uint64(len(base)))
	header = binary.AppendUvarint(header, uint64(len(base)))
	header = append(header, 0x80)
	got, err := applyDelta(t.Context(), base, string(header), 1<<20)
	require.NoError(t, err)
	require.Equal(t, base, got)
	for _, delta := range []string{
		"", "\x80", "\x03", "\x04\x00", "\x03\x04\x04abc",
		"\x03\x01\x00", "\x03\x01\x01x\x01y", "\x03\x03\x91\x02\x03",
		"\x03\x03\x90", "\x03\x04\x90\x03", "\x03\x00\x00",
		"\x03" + strings.Repeat("\xff", 10),
	} {
		_, err := applyDelta(t.Context(), "abc", delta, 1<<20)
		require.Error(t, err, "%x", delta)
	}
	_, err = applyDelta(t.Context(), "abc", "\x03\x04\x90\x03\x01d", 3)
	require.ErrorContains(t, err, "limit")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = applyDelta(ctx, "abc", "\x03\x03\x90\x03", 10)
	require.ErrorIs(t, err, context.Canceled)
}

func FuzzApplyDelta(f *testing.F) {
	f.Add("abc", "\x03\x04\x90\x03\x01d")
	f.Add("abcdef", "\x06\x03\x91\x02\x03")
	f.Add("abc", "\x03\x00")
	f.Fuzz(func(t *testing.T, base, delta string) {
		if len(base) == 0 || len(base) > 65536 || len(delta) > 65536 {
			t.Skip()
		}
		// Keep the reference decoder's allocation bounded too.
		_, n := binary.Uvarint([]byte(delta))
		if n <= 0 {
			return
		}
		size, n := binary.Uvarint([]byte(delta[n:]))
		if n <= 0 || size > 1<<20 {
			return
		}
		want, wantErr := packfile.PatchDelta([]byte(base), []byte(delta))
		got, err := applyDelta(t.Context(), base, delta, 1<<20)
		if wantErr != nil {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		require.Equal(t, string(want), got)
	})
}
