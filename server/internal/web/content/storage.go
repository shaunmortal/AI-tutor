package content

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// bucket is private: files are only reached through short-lived signed links.
const bucket = "course-sources"

// signedFor is how long an opened file link works: long enough to read, short enough not to leak.
const signedFor = time.Hour

// storage is the four Supabase Storage REST calls this feature needs, with the secret key.
type storage struct {
	url, key string
	http     *http.Client
}

func (s *storage) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if s.key == "" {
		return nil, httpx.E(http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", "File storage is not configured on this server.")
	}
	req, err := http.NewRequestWithContext(ctx, method, s.url+"/storage/v1/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", s.key)
	req.Header.Set("Authorization", "Bearer "+s.key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("storage %s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (s *storage) upload(ctx context.Context, path string, file []byte, contentType string) error {
	_, err := s.do(ctx, http.MethodPost, "object/"+bucket+"/"+path, contentType, file)
	return err
}

func (s *storage) download(ctx context.Context, path string) ([]byte, error) {
	return s.do(ctx, http.MethodGet, "object/"+bucket+"/"+path, "", nil)
}

func (s *storage) remove(ctx context.Context, path string) error {
	body, _ := json.Marshal(map[string][]string{"prefixes": {path}})
	_, err := s.do(ctx, http.MethodDelete, "object/"+bucket, "application/json", body)
	return err
}

func (s *storage) signedURL(ctx context.Context, path string) (string, error) {
	body, _ := json.Marshal(map[string]int{"expiresIn": int(signedFor.Seconds())})
	raw, err := s.do(ctx, http.MethodPost, "object/sign/"+bucket+"/"+path, "application/json", body)
	if err != nil {
		return "", err
	}
	var out struct {
		SignedURL string `json:"signedURL"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.SignedURL == "" {
		return "", fmt.Errorf("storage sign %s: unexpected reply %s", path, raw)
	}
	return s.url + "/storage/v1" + out.SignedURL, nil
}
