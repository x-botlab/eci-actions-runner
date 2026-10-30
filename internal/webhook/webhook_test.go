package webhook

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

const secret = "s3cr3t"

func newHandler(t *testing.T) (*Handler, *int) {
	t.Helper()

	kicks := 0
	handler, err := New([]byte(secret), []string{"x-botlab/eci-actions-runner"},
		func() { kicks++ }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler, &kicks
}

func post(t *testing.T, handler http.Handler, event string, body []byte, signature string) int {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	request.Header.Set("X-GitHub-Event", event)
	if signature != "" {
		request.Header.Set("X-Hub-Signature-256", signature)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}

func TestQueuedDeliveryKicks(t *testing.T) {
	handler, kicks := newHandler(t)
	body := []byte(`{"action":"queued","repository":{"full_name":"x-botlab/eci-actions-runner"},
		"workflow_job":{"labels":["self-hosted","eci-pool-short"],"status":"queued"}}`)

	if code := post(t, handler, "workflow_job", body, Sign([]byte(secret), body)); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if *kicks != 1 {
		t.Fatalf("kicks = %d, want 1", *kicks)
	}
}

func TestBadSignatureIsRejected(t *testing.T) {
	handler, kicks := newHandler(t)
	body := []byte(`{"action":"queued","repository":{"full_name":"x-botlab/eci-actions-runner"}}`)

	cases := map[string]string{
		"wrong secret": Sign([]byte("other"), body),
		"no prefix":    "deadbeef",
		"bad hex":      "sha256=zzzz",
		"empty":        "",
	}
	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			if code := post(t, handler, "workflow_job", body, signature); code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", code)
			}
		})
	}
	if *kicks != 0 {
		t.Fatalf("kicks = %d, want 0", *kicks)
	}
}

func TestTamperedBodyIsRejected(t *testing.T) {
	handler, kicks := newHandler(t)
	signed := []byte(`{"action":"queued","repository":{"full_name":"x-botlab/eci-actions-runner"}}`)
	signature := Sign([]byte(secret), signed)
	tampered := []byte(`{"action":"queued","repository":{"full_name":"x-botlab/eci-actions-runner"},"x":1}`)

	if code := post(t, handler, "workflow_job", tampered, signature); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if *kicks != 0 {
		t.Fatalf("kicks = %d, want 0", *kicks)
	}
}

func TestOtherEventsAreAcknowledgedWithoutKicking(t *testing.T) {
	handler, kicks := newHandler(t)
	body := []byte(`{"zen":"hi"}`)

	if code := post(t, handler, "ping", body, Sign([]byte(secret), body)); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if *kicks != 0 {
		t.Fatalf("kicks = %d, want 0", *kicks)
	}
}

func TestUnmanagedRepositoryIsIgnored(t *testing.T) {
	handler, kicks := newHandler(t)
	body := []byte(`{"action":"queued","repository":{"full_name":"someone/else"}}`)

	if code := post(t, handler, "workflow_job", body, Sign([]byte(secret), body)); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if *kicks != 0 {
		t.Fatalf("kicks = %d, want 0", *kicks)
	}
}

func TestRepositoryMatchIsCaseInsensitive(t *testing.T) {
	handler, kicks := newHandler(t)
	body := []byte(`{"action":"queued","repository":{"full_name":"X-BotLab/ECI-Actions-Runner"}}`)

	post(t, handler, "workflow_job", body, Sign([]byte(secret), body))
	if *kicks != 1 {
		t.Fatalf("kicks = %d, want 1", *kicks)
	}
}

func TestIrrelevantActionDoesNotKick(t *testing.T) {
	handler, kicks := newHandler(t)
	body := []byte(`{"action":"waiting","repository":{"full_name":"x-botlab/eci-actions-runner"}}`)

	if code := post(t, handler, "workflow_job", body, Sign([]byte(secret), body)); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if *kicks != 0 {
		t.Fatalf("kicks = %d, want 0", *kicks)
	}
}

func TestOnlyPostIsAllowed(t *testing.T) {
	handler, _ := newHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

func TestUnparseableSignedDeliveryIsAccepted(t *testing.T) {
	handler, kicks := newHandler(t)
	body := []byte(`not json`)

	if code := post(t, handler, "workflow_job", body, Sign([]byte(secret), body)); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if *kicks != 0 {
		t.Fatalf("kicks = %d, want 0", *kicks)
	}
}

func TestNewRequiresSecret(t *testing.T) {
	if _, err := New(nil, []string{"a/b"}, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected an error without a secret")
	}
}
