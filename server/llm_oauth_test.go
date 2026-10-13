package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOAuthCredentialEncryptionRoundTrip(t *testing.T) {
	s := &Server{jwtKey: []byte("test-only-signing-key")}
	ciphertext, err := s.oauthEncrypt("secret-access-token")
	if err != nil {
		t.Fatal(err)
	}
	if ciphertext == "secret-access-token" || !strings.HasPrefix(ciphertext, "v1.") {
		t.Fatalf("unexpected ciphertext %q", ciphertext)
	}
	plain, err := s.oauthDecrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "secret-access-token" {
		t.Fatalf("round trip = %q", plain)
	}

	other := &Server{jwtKey: []byte("different-key")}
	if _, err := other.oauthDecrypt(ciphertext); err == nil {
		t.Fatal("decrypt with a different key unexpectedly succeeded")
	}
}

func TestSIWCRedirectURILoopbackOnly(t *testing.T) {
	t.Setenv("ARTEX_SIWC_REDIRECT_URI", "https://example.com/api/llm/oauth/callback")
	if _, err := siwcRedirectURI(nil); err == nil {
		t.Fatal("non-loopback redirect unexpectedly accepted")
	}
}

func TestSIWCCallbackRejectsConcurrentOrReplayedAttempt(t *testing.T) {
	const state = "state-value"
	s := &Server{oauthPending: map[string]*siwcPending{
		state: {State: state, StartedAt: time.Now(), Processing: true},
	}}
	r := httptest.NewRequest("GET", "/api/llm/oauth/callback?state="+state+"&code=replayed", nil)
	w := httptest.NewRecorder()
	s.llmOAuthCallback(w, r)
	if !strings.Contains(w.Body.String(), "이미 처리 중이거나 완료된") {
		t.Fatalf("unexpected callback response: %s", w.Body.String())
	}
}
