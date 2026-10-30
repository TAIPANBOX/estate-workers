// Package github is the push service's only way to GitHub: a GitHub App
// that mints a token for one repository at a time, and the one call that
// opens a pull request. Nothing here merges, tags or releases.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxBody bounds every response body read from the API.
const MaxBody = 1 << 20

type App struct {
	ID      string
	Key     *rsa.PrivateKey
	BaseURL string
	Client  *http.Client
	Now     func() time.Time
}

type NewPR struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

// ParseKey reads the App's private key in PKCS#1 or PKCS#8 PEM form.
func ParseKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github: no PEM block in the App key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: App key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: App key is not RSA")
	}
	return rk, nil
}

func (a App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// JWT signs the short-lived token that authenticates the App itself.
// Issued a minute in the past for clock drift, valid nine minutes.
func (a App) JWT() (string, error) {
	if a.Key == nil || a.ID == "" {
		return "", errors.New("github: App id or key missing")
	}
	now := a.now().Unix()
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 540, "iss": a.ID})
	if err != nil {
		return "", err
	}
	signing := head + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (a App) do(ctx context.Context, method, path, auth string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "estate-workers")
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("github %s %s: HTTP %d: %.200s", method, path, resp.StatusCode, e.Message)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("github %s %s: %w", method, path, err)
		}
	}
	return nil
}

// RepoToken mints an installation token limited to one repository and to
// contents and pull requests, whatever the App itself may hold.
func (a App) RepoToken(ctx context.Context, owner, repo string) (string, error) {
	jwt, err := a.JWT()
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	p := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if err := a.do(ctx, http.MethodGet, p, "Bearer "+jwt, nil, &inst); err != nil {
		return "", err
	}
	if inst.ID <= 0 {
		return "", errors.New("github: no installation id for the repository")
	}
	req := map[string]any{
		"repositories": []string{repo},
		"permissions":  map[string]string{"contents": "write", "pull_requests": "write"},
	}
	var tok struct {
		Token string `json:"token"`
	}
	p = fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID)
	if err := a.do(ctx, http.MethodPost, p, "Bearer "+jwt, req, &tok); err != nil {
		return "", err
	}
	if tok.Token == "" {
		return "", errors.New("github: empty installation token")
	}
	return tok.Token, nil
}

// CreatePR opens a pull request and returns its web address.
func (a App) CreatePR(ctx context.Context, token, owner, repo string, pr NewPR) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	p := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls"
	if err := a.do(ctx, http.MethodPost, p, "Bearer "+token, pr, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.HTMLURL, "https://") {
		return "", fmt.Errorf("github: pull request created but its address %q is not https", out.HTMLURL)
	}
	return out.HTMLURL, nil
}

// AuthConfig is the git config that sends the token to one host only, as a
// header, so it never appears in argv or a URL.
func AuthConfig(host, token string) [][2]string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return [][2]string{{"http.https://" + host + "/.extraHeader", "Authorization: Basic " + basic}}
}
