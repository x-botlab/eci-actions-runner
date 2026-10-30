package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// writeKey generates an RSA key and writes it where NewAppClient expects it.
func writeKey(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path, key
}

func TestAppJWTCarriesIssuerAndShortExpiry(t *testing.T) {
	path, key := writeKey(t)
	client, err := NewAppClient(Config{AppID: 4242, PrivateKeyPath: path})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	now := time.Now()
	signed, err := client.AppJWT(now)
	if err != nil {
		t.Fatalf("app jwt: %v", err)
	}

	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(signed, claims, func(*jwt.Token) (any, error) {
		return &key.PublicKey, nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("parse jwt: %v", err)
	}
	if claims.Issuer != "4242" {
		t.Fatalf("issuer = %q, want 4242", claims.Issuer)
	}
	if lifetime := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time); lifetime > 10*time.Minute {
		t.Fatalf("lifetime %s exceeds the 10 minute limit", lifetime)
	}
}

func TestLoadPrivateKeyAcceptsPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	loaded, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Equal(key) {
		t.Fatal("loaded key differs from the generated one")
	}
}

func TestAppJWTUsesClientIDAsIssuer(t *testing.T) {
	path, key := writeKey(t)
	client, err := NewAppClient(Config{ClientID: "Iv1.abc123", PrivateKeyPath: path})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	signed, err := client.AppJWT(time.Now())
	if err != nil {
		t.Fatalf("app jwt: %v", err)
	}

	claims := &jwt.RegisteredClaims{}
	if _, err := jwt.ParseWithClaims(signed, claims, func(*jwt.Token) (any, error) {
		return &key.PublicKey, nil
	}); err != nil {
		t.Fatalf("parse jwt: %v", err)
	}
	if claims.Issuer != "Iv1.abc123" {
		t.Fatalf("issuer = %q, want the client id", claims.Issuer)
	}
}

func TestNewAppClientRequiresAnIdentity(t *testing.T) {
	path, _ := writeKey(t)

	if _, err := NewAppClient(Config{PrivateKeyPath: path}); err == nil {
		t.Fatal("expected an error without an app id or client id")
	}
}

// apiStub serves the handful of endpoints the client uses and counts token mints.
type apiStub struct {
	tokenMints atomic.Int32
	handler    http.Handler
}

func newAPIStub(t *testing.T) (*apiStub, *AppClient) {
	t.Helper()

	path, _ := writeKey(t)
	stub := &apiStub{}
	mux := http.NewServeMux()

	mux.HandleFunc("/app/installations/7/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		stub.tokenMints.Add(1)
		writeJSON(t, w, `{"token":"ghs_installation",
			"expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`",
			"permissions":{"actions":"read","administration":"write","metadata":"read"}}`)
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("status") {
		case "queued":
			writeJSON(t, w, `{"total_count":1,"workflow_runs":[{"id":11}]}`)
		case "in_progress":
			writeJSON(t, w, `{"total_count":1,"workflow_runs":[{"id":12}]}`)
		default:
			t.Errorf("unexpected status filter %q", r.URL.Query().Get("status"))
			writeJSON(t, w, `{"total_count":0,"workflow_runs":[]}`)
		}
	})
	mux.HandleFunc("/repos/o/r/actions/runs/11/jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"total_count":1,"jobs":[
			{"id":100,"run_id":11,"status":"queued","labels":["self-hosted","eci-pool-short"],
			 "created_at":"2026-01-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/repos/o/r/actions/runs/12/jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"total_count":1,"jobs":[
			{"id":101,"run_id":12,"status":"in_progress","labels":["self-hosted"]}]}`)
	})
	mux.HandleFunc("/repos/o/r/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("jit config expects POST, got %s", r.Method)
		}
		writeJSON(t, w, `{"runner":{"id":5,"name":"eci-pool-short-1"},"encoded_jit_config":"encoded"}`)
	})
	mux.HandleFunc("/repos/o/r/actions/runners", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"total_count":2,"runners":[
			{"id":1,"name":"eci-pool-short-a","status":"offline","busy":false,
			 "labels":[{"name":"self-hosted"}]},
			{"id":2,"name":"other-runner","status":"online","busy":true}]}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stub.handler = mux

	client, err := NewAppClient(Config{AppID: 1, PrivateKeyPath: path, BaseURL: server.URL + "/"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return stub, client
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestQueuedJobsReturnsOnlyQueuedJobs(t *testing.T) {
	_, client := newAPIStub(t)

	jobs, err := client.QueuedJobs(context.Background(), Repo{Owner: "o", Name: "r", InstallationID: 7})
	if err != nil {
		t.Fatalf("queued jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1: in_progress jobs must be skipped", len(jobs))
	}
	if jobs[0].ID != 100 || jobs[0].RunID != 11 {
		t.Fatalf("job = %+v", jobs[0])
	}
	if !jobs[0].CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("created_at = %s", jobs[0].CreatedAt)
	}
}

func TestInstallationTokenIsCached(t *testing.T) {
	stub, client := newAPIStub(t)
	repo := Repo{Owner: "o", Name: "r", InstallationID: 7}

	for range 3 {
		if _, err := client.QueuedJobs(context.Background(), repo); err != nil {
			t.Fatalf("queued jobs: %v", err)
		}
	}
	if mints := stub.tokenMints.Load(); mints != 1 {
		t.Fatalf("minted %d installation tokens, want 1", mints)
	}
}

func TestInstallationTokenIsRenewedBeforeExpiry(t *testing.T) {
	stub, client := newAPIStub(t)
	repo := Repo{Owner: "o", Name: "r", InstallationID: 7}

	if _, err := client.QueuedJobs(context.Background(), repo); err != nil {
		t.Fatalf("queued jobs: %v", err)
	}

	// Jump past the refresh margin: the cached token is no longer usable.
	client.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := client.QueuedJobs(context.Background(), repo); err != nil {
		t.Fatalf("queued jobs: %v", err)
	}
	if mints := stub.tokenMints.Load(); mints != 2 {
		t.Fatalf("minted %d tokens, want 2 after expiry", mints)
	}
}

func TestCreateJITConfig(t *testing.T) {
	_, client := newAPIStub(t)

	config, err := client.CreateJITConfig(context.Background(),
		Repo{Owner: "o", Name: "r", InstallationID: 7}, "eci-pool-short-1", []string{"eci-pool-short"})
	if err != nil {
		t.Fatalf("jit config: %v", err)
	}
	if config.Encoded != "encoded" {
		t.Fatalf("encoded = %q", config.Encoded)
	}
	if config.RunnerID != 5 || config.RunnerName != "eci-pool-short-1" {
		t.Fatalf("runner = %d/%q", config.RunnerID, config.RunnerName)
	}
}

func TestTokenReportsPermissions(t *testing.T) {
	_, client := newAPIStub(t)

	token, err := client.Token(context.Background(), 7)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if got := token.Permissions["administration"]; got != "write" {
		t.Fatalf("administration = %q, want write", got)
	}
	if got := token.Permissions["actions"]; got != "read" {
		t.Fatalf("actions = %q, want read", got)
	}
	if token.Expires.IsZero() {
		t.Fatal("expected an expiry")
	}
}

func TestRunnersFiltersByPrefix(t *testing.T) {
	_, client := newAPIStub(t)

	runners, err := client.Runners(context.Background(),
		Repo{Owner: "o", Name: "r", InstallationID: 7}, "eci-pool-short-")
	if err != nil {
		t.Fatalf("runners: %v", err)
	}
	if len(runners) != 1 || runners[0].Name != "eci-pool-short-a" {
		t.Fatalf("runners = %+v", runners)
	}
	if runners[0].Status != "offline" || runners[0].Busy {
		t.Fatalf("runner state = %+v", runners[0])
	}
}

func TestQueuedJobsRequiresInstallation(t *testing.T) {
	_, client := newAPIStub(t)

	_, err := client.QueuedJobs(context.Background(), Repo{Owner: "o", Name: "r"})
	if err == nil || !strings.Contains(err.Error(), "installation id") {
		t.Fatalf("expected an installation id error, got %v", err)
	}
}
