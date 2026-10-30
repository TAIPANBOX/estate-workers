package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var fixed = time.Unix(1_800_000_000, 0)

func verifyJWT(t *testing.T, jwt string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestJWTIsSignedAndShortLived(t *testing.T) {
	k := testKey(t)
	a := App{ID: "123456", Key: k, Now: func() time.Time { return fixed }}
	jwt, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	c := verifyJWT(t, jwt, &k.PublicKey)
	if c["iss"] != "123456" {
		t.Errorf("iss %v", c["iss"])
	}
	iat, exp := int64(c["iat"].(float64)), int64(c["exp"].(float64))
	if iat > fixed.Unix() || exp-fixed.Unix() > 600 || exp <= fixed.Unix() {
		t.Errorf("iat %d exp %d around %d", iat, exp, fixed.Unix())
	}
	if _, err := (App{ID: "1"}).JWT(); err == nil {
		t.Error("JWT without a key succeeded")
	}
}

func TestRepoTokenAsksForOneRepoAndTwoPermissions(t *testing.T) {
	k := testKey(t)
	var tokenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("no bearer on %s", r.URL.Path)
		}
		verifyJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &k.PublicKey)
		switch r.URL.Path {
		case "/repos/TAIPANBOX/architecture/installation":
			io.WriteString(w, `{"id": 42}`)
		case "/app/installations/42/access_tokens":
			if r.Method != http.MethodPost {
				t.Errorf("method %s", r.Method)
			}
			json.NewDecoder(r.Body).Decode(&tokenBody)
			io.WriteString(w, `{"token": "ghs_test_token_value", "expires_at": "2027-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := App{ID: "7", Key: k, BaseURL: srv.URL, Client: srv.Client(), Now: time.Now}
	tok, err := a.RepoToken(ctx, "TAIPANBOX", "architecture")
	if err != nil || tok != "ghs_test_token_value" {
		t.Fatalf("token %q err %v", tok, err)
	}
	repos, _ := tokenBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "architecture" {
		t.Errorf("repositories %v", tokenBody["repositories"])
	}
	perms, _ := tokenBody["permissions"].(map[string]any)
	if len(perms) != 2 || perms["contents"] != "write" || perms["pull_requests"] != "write" {
		t.Errorf("permissions %v", perms)
	}
}

func TestRepoTokenErrors(t *testing.T) {
	k := testKey(t)
	cases := map[string]http.HandlerFunc{
		"no installation": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		},
		"zero id": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"id":0}`) },
		"empty token": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/installation") {
				io.WriteString(w, `{"id":1}`)
				return
			}
			io.WriteString(w, `{"token":""}`)
		},
		"bad json": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{`) },
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		a := App{ID: "7", Key: k, BaseURL: srv.URL, Client: srv.Client()}
		if _, err := a.RepoToken(ctx, "o", "r"); err == nil {
			t.Errorf("%s: no error", name)
		}
		srv.Close()
	}
}

func TestCreatePROpensOneAndReturnsItsAddress(t *testing.T) {
	var got NewPR
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/TAIPANBOX/architecture/pulls" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		io.WriteString(w, `{"html_url":"https://github.com/TAIPANBOX/architecture/pull/9"}`)
	}))
	defer srv.Close()
	a := App{BaseURL: srv.URL, Client: srv.Client()}
	u, err := a.CreatePR(ctx, "tok", "TAIPANBOX", "architecture", NewPR{Title: "t", Head: "worker/x/1", Base: "main", Body: "b"})
	if err != nil || u != "https://github.com/TAIPANBOX/architecture/pull/9" {
		t.Fatalf("url %q err %v", u, err)
	}
	if auth != "Bearer tok" || got.Head != "worker/x/1" || got.Base != "main" {
		t.Errorf("auth %q pr %+v", auth, got)
	}
}

func TestCreatePRRefusesANonHTTPSAddressAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"html_url":"javascript:alert(1)"}`)
	}))
	defer srv.Close()
	a := App{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := a.CreatePR(ctx, "t", "o", "r", NewPR{}); err == nil {
		t.Error("a non-https address was accepted")
	}
	a.BaseURL = "http://127.0.0.1:1"
	if _, err := a.CreatePR(ctx, "t", "o", "r", NewPR{}); err == nil {
		t.Error("an unreachable API gave no error")
	}
}

func TestParseKeyReadsPKCS1AndPKCS8(t *testing.T) {
	k := testKey(t)
	p1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	der8, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der8})
	for _, p := range [][]byte{p1, p8} {
		got, err := ParseKey(p)
		if err != nil || !got.Equal(k) {
			t.Errorf("ParseKey: %v", err)
		}
	}
	for _, bad := range [][]byte{nil, []byte("not pem"), pem.EncodeToMemory(&pem.Block{Type: "X", Bytes: []byte("junk")})} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) succeeded", bad)
		}
	}
}

func TestAuthConfigScopesTheHeaderToOneHost(t *testing.T) {
	c := AuthConfig("github.com", "tok")
	if len(c) != 1 || c[0][0] != "http.https://github.com/.extraHeader" {
		t.Fatalf("config %v", c)
	}
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	if c[0][1] != want {
		t.Errorf("header %q", c[0][1])
	}
}
