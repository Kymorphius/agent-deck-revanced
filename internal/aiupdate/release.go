package aiupdate

// matrix/hardened: release requests use only public OpenAI metadata and verified full packages.
import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const maxArchiveBytes int64 = 1 << 30

type releaseAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type releaseMetadata struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

func releaseClient() *http.Client {
	// matrix/hardened: a dedicated client has no cookie jar or authentication provider.
	return &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.Host != "releases.openai.com" || req.URL.User != nil {
			return errors.New("release redirect left the trusted OpenAI release host")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Cookie")
		return nil
	}}
}

func (m *Manager) fetch(ctx context.Context, suffix string, writer io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+suffix, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream, application/json")
	// matrix/hardened: do not consult GITHUB_TOKEN, GH_TOKEN, gh, Codex auth, or netrc.
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch Codex release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Codex release server returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return errors.New("Codex release response exceeds size limit")
	}
	n, err := io.Copy(writer, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("read Codex release response: %w", err)
	}
	if n > limit {
		return errors.New("Codex release response exceeds size limit")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != n {
		return errors.New("truncated Codex release response")
	}
	return nil
}

func (release releaseMetadata) version() string { return strings.TrimPrefix(release.Tag, "rust-v") }

func (m *Manager) metadata(ctx context.Context, suffix string) (releaseMetadata, error) {
	var body strings.Builder
	if err := m.fetch(ctx, suffix, &body, 4<<20); err != nil {
		return releaseMetadata{}, err
	}
	var release releaseMetadata
	if err := json.Unmarshal([]byte(body.String()), &release); err != nil {
		return release, fmt.Errorf("invalid Codex release metadata: %w", err)
	}
	if release.Tag != "rust-v"+release.version() || !stableVersion.MatchString(release.version()) {
		return release, errors.New("Codex release metadata did not select a stable version")
	}
	return release, nil
}

func (m *Manager) latest(ctx context.Context) (releaseMetadata, error) {
	return m.metadata(ctx, "/channels/latest")
}

func (release releaseMetadata) digest(name string) (string, error) {
	var result string
	for _, asset := range release.Assets {
		if asset.Name != name {
			continue
		}
		if result != "" {
			return "", fmt.Errorf("duplicate Codex release asset %s", name)
		}
		result = strings.TrimPrefix(asset.Digest, "sha256:")
		if len(result) != 64 || asset.Digest != "sha256:"+result {
			return "", fmt.Errorf("missing SHA-256 for Codex asset %s", name)
		}
		if _, err := hex.DecodeString(result); err != nil {
			return "", fmt.Errorf("invalid SHA-256 for Codex asset %s", name)
		}
	}
	if result == "" {
		return "", fmt.Errorf("Codex release lacks full package asset %s", name)
	}
	return strings.ToLower(result), nil
}

func checksumDigest(body, name string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(body))
	var found string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if found != "" {
			return "", errors.New("duplicate package checksum entry")
		}
		found = strings.ToLower(fields[0])
		if len(found) != 64 {
			return "", errors.New("invalid package checksum")
		}
		if _, err := hex.DecodeString(found); err != nil {
			return "", errors.New("invalid package checksum")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("package checksum manifest lacks requested archive")
	}
	return found, nil
}

func (m *Manager) download(ctx context.Context, release releaseMetadata, output string) error {
	name := "codex-package-" + m.target + ".tar.gz"
	archiveDigest, err := release.digest(name)
	if err != nil {
		return err
	}
	manifestDigest, err := release.digest("codex-package_SHA256SUMS")
	if err != nil {
		return err
	}
	var manifest strings.Builder
	suffix := "/releases/" + release.version() + "/"
	if err := m.fetch(ctx, suffix+"codex-package_SHA256SUMS", &manifest, 1<<20); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(manifest.String()))
	if hex.EncodeToString(hash[:]) != manifestDigest {
		return errors.New("Codex package checksum manifest SHA-256 mismatch")
	}
	manifestArchiveDigest, err := checksumDigest(manifest.String(), name)
	if err != nil {
		return err
	}
	if manifestArchiveDigest != archiveDigest {
		return errors.New("Codex package metadata and checksum manifest disagree")
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	err = m.fetch(ctx, suffix+name, io.MultiWriter(file, hasher), maxArchiveBytes)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hasher.Sum(nil)) != archiveDigest {
		return errors.New("Codex package archive SHA-256 mismatch")
	}
	return nil
}
