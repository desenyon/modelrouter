package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Artifact is one pinned file of the default embedder.
type Artifact struct {
	Name   string
	SHA256 string
}

// Default embedder: minishlab/potion-base-8M (MIT), pinned to a revision so a
// silent upstream change can't alter routing behaviour.
const (
	DefaultName     = "potion-base-8M"
	DefaultRepo     = "minishlab/potion-base-8M"
	DefaultRevision = "bf8b056651a2c21b8d2565580b8569da283cab23"
)

// DefaultArtifacts lists the pinned files and their digests.
var DefaultArtifacts = []Artifact{
	{Name: "model.safetensors", SHA256: "f65d0f325faadc1e121c319e2faa41170d3fa07d8c89abd48ca5358d9a223de2"},
	{Name: "tokenizer.json", SHA256: "e67e803f624fb4d67dea1c730d06e1067e1b14d830e2c2202569e3ef0f70bb50"},
	{Name: "config.json", SHA256: "2a6ac0e9aaa356a68a5688070db78fc3a464fefe85d2f06a1905ce3718687553"},
}

// DefaultDir returns the per-user cache location for the default embedder.
func DefaultDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "modelrouter", DefaultName)
}

// Present reports whether every pinned artifact exists in dir with the right digest.
func Present(dir string) error {
	for _, a := range DefaultArtifacts {
		sum, err := fileSHA256(filepath.Join(dir, a.Name))
		if err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
		if sum != a.SHA256 {
			return fmt.Errorf("%s: checksum mismatch", a.Name)
		}
	}
	return nil
}

// Fetch downloads missing or corrupt artifacts into dir, verifying digests.
func Fetch(ctx context.Context, dir string, logf func(string, ...any)) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, a := range DefaultArtifacts {
		dst := filepath.Join(dir, a.Name)
		if sum, err := fileSHA256(dst); err == nil && sum == a.SHA256 {
			continue
		}
		url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", DefaultRepo, DefaultRevision, a.Name)
		if logf != nil {
			logf("embedder: downloading %s", url)
		}
		if err := download(ctx, client, url, dst, a.SHA256); err != nil {
			return fmt.Errorf("download %s: %w", a.Name, err)
		}
	}
	return nil
}

func download(ctx context.Context, c *http.Client, url, dst, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch: got %s", got)
	}
	return os.Rename(tmp.Name(), dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Ensure loads the embedder from dir, downloading it first when allowed.
// The router requires an embedder: callers should treat an error as fatal.
func Ensure(ctx context.Context, dir string, autoDownload bool, logf func(string, ...any)) (*Model, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := Present(dir); err != nil {
		if !autoDownload {
			return nil, fmt.Errorf("embedder missing at %s (%v); run `modelrouter embedder fetch`", dir, err)
		}
		if err := Fetch(ctx, dir, logf); err != nil {
			return nil, err
		}
	}
	return Load(dir)
}
