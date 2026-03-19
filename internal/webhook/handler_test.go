package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v84/github"
	"github.com/iamangus/opendev-git/internal/config"
)

func newTestHandler(secret string) *Handler {
	return &Handler{
		config: &config.Config{
			GitHubWebhookSecret: secret,
			DesignatedLabel:     "opendev-git",
		},
	}
}

func signBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignatureValid(t *testing.T) {
	h := newTestHandler("secret")
	body := []byte(`{"action":"opened"}`)
	sig := signBody(body, "secret")
	if !h.verifySignature(body, sig) {
		t.Error("expected valid signature to pass")
	}
}

func TestVerifySignatureBad(t *testing.T) {
	h := newTestHandler("secret")
	body := []byte(`{"action":"opened"}`)
	if h.verifySignature(body, "sha256=badhash") {
		t.Error("expected bad signature to fail")
	}
}

func TestVerifySignatureMissingPrefix(t *testing.T) {
	h := newTestHandler("secret")
	body := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	rawSig := hex.EncodeToString(mac.Sum(nil))
	if h.verifySignature(body, rawSig) {
		t.Error("expected missing sha256= prefix to fail")
	}
}

func TestVerifySignatureNoSecret(t *testing.T) {
	h := newTestHandler("")
	if !h.verifySignature([]byte("anything"), "") {
		t.Error("expected no-secret mode to pass all requests")
	}
}

func TestServeHTTPMethodNotAllowed(t *testing.T) {
	h := newTestHandler("")
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestServeHTTPInvalidSignature(t *testing.T) {
	h := newTestHandler("secret")
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=wrongsig")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for invalid signature, got %d", rr.Code)
	}
}

func TestIssuePayloadParsing(t *testing.T) {
	payload := issuesPayload{
		Action: "labeled",
		Issue: &github.Issue{
			Number: github.Ptr(1),
			Labels: []*github.Label{
				{Name: github.Ptr("opendev-git")},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var parsed issuesPayload
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Action != "labeled" {
		t.Errorf("action = %q, want 'labeled'", parsed.Action)
	}
	if parsed.Issue == nil {
		t.Fatal("issue is nil")
	}
	if parsed.Issue.GetNumber() != 1 {
		t.Errorf("issue number = %d, want 1", parsed.Issue.GetNumber())
	}
}
