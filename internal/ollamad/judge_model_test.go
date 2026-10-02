package ollamad

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadVerifiedFile(t *testing.T) {
	payload := []byte("pinned judge weights")
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	t.Run("correct digest installs", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "m.gguf")
		require.NoError(t, downloadVerifiedFile(context.Background(), srv.URL, good, dest))
		got, err := os.ReadFile(dest)
		require.NoError(t, err)
		require.Equal(t, payload, got)
		require.NoFileExists(t, dest+".part")
	})

	t.Run("wrong digest refuses and leaves no file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "m.gguf")
		err := downloadVerifiedFile(context.Background(), srv.URL, "deadbeef", dest)
		require.ErrorContains(t, err, "checksum mismatch")
		require.NoFileExists(t, dest, "a model that fails verification must not be left where the loader finds it")
		require.NoFileExists(t, dest+".part")
	})

	t.Run("http error refuses", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
		defer bad.Close()
		dest := filepath.Join(t.TempDir(), "m.gguf")
		require.Error(t, downloadVerifiedFile(context.Background(), bad.URL, good, dest))
		require.NoFileExists(t, dest)
	})
}

// The pinned judge constants are internally consistent: a 64-hex sha256 and an HTTPS URL that carries the tag.
func TestJudgePinConsistency(t *testing.T) {
	require.Len(t, judgeGGUFSHA256, 64)
	require.Contains(t, judgeGGUFURL, "https://")
	require.Contains(t, judgeGGUFURL, judgeGGUFName)
}
