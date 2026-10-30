// Package webhook turns a GitHub workflow_job delivery into a reconcile request.
//
// It is a fast path, not a source of truth: a delivery only asks the scheduler to
// look at the queue immediately. Dropping one costs at most one poll interval,
// which is why the handler does no bookkeeping and never blocks for long.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Intent is the only thing the scheduler is told: look now. Carrying job details
// here would duplicate state that GitHub already owns.
type Intent struct {
	Repo   string
	Action string
	Labels []string
}

// Handler answers GitHub webhook deliveries.
type Handler struct {
	secret []byte
	// repos limits kicks to repositories this service manages, so an unrelated
	// installation cannot make the scheduler reconcile on every delivery.
	repos  map[string]struct{}
	kick   func()
	logger *slog.Logger
}

// New builds a handler. repos holds "owner/name" entries.
func New(secret []byte, repos []string, kick func(), logger *slog.Logger) (*Handler, error) {
	if len(secret) == 0 {
		return nil, errors.New("webhook: a secret is required to verify deliveries")
	}
	if kick == nil {
		return nil, errors.New("webhook: a kick function is required")
	}

	managed := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		managed[strings.ToLower(repo)] = struct{}{}
	}
	return &Handler{secret: secret, repos: managed, kick: kick, logger: logger}, nil
}

// maxBody bounds what is read: a delivery is authenticated, but reading an
// arbitrary amount before checking the signature is still a waste.
const maxBody = 10 << 20

// delivery is the part of the payload that decides whether to reconcile.
type delivery struct {
	Action      string `json:"action"`
	Repository  repo   `json:"repository"`
	WorkflowJob job    `json:"workflow_job"`
}

type repo struct {
	FullName string `json:"full_name"`
}

type job struct {
	Labels []string `json:"labels"`
	Status string   `json:"status"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	if !VerifySignature(h.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		h.logger.Warn("rejected a delivery with a bad signature",
			"delivery", r.Header.Get("X-GitHub-Delivery"),
			"event", r.Header.Get("X-GitHub-Event"))
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	// A ping is what GitHub sends when the hook is created, and deserves a 200.
	if event != "workflow_job" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var payload delivery
	if err := json.Unmarshal(body, &payload); err != nil {
		// Signed but unreadable: accept it anyway. Returning an error would only
		// make GitHub retry a payload this service cannot use either way.
		h.logger.Warn("cannot parse a signed delivery", "err", err)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	intent := Intent{Repo: payload.Repository.FullName, Action: payload.Action, Labels: payload.WorkflowJob.Labels}
	if !h.manages(intent.Repo) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Only states that can change the desired size matter. A delivery arrives for
	// every job transition, so anything else is noise.
	switch intent.Action {
	case "queued", "in_progress", "completed":
		h.kick()
		h.logger.Info("webhook received, reconciling now",
			"repo", intent.Repo, "action", intent.Action, "labels", intent.Labels,
			"delivery", r.Header.Get("X-GitHub-Delivery"))
	default:
		w.WriteHeader(http.StatusOK)
		return
	}

	// Answer immediately: GitHub treats a response slower than ten seconds as a
	// failed delivery, and reconcile already runs in the scheduler goroutine.
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) manages(repo string) bool {
	_, ok := h.repos[strings.ToLower(repo)]
	return ok
}

// VerifySignature checks X-Hub-Signature-256 against the raw body. The comparison
// is constant time, and a missing or malformed header is a rejection rather than
// a skip: accepting unsigned deliveries would let anyone trigger reconciles.
func VerifySignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}

	provided, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

// Sign is the counterpart of VerifySignature, used by tests and by a setup check.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
