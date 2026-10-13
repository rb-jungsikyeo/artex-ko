package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/golang-jwt/jwt/v5"
)

const (
	siwcIssuer       = "https://auth.openai.com"
	siwcAuthorizeURL = siwcIssuer + "/api/accounts/authorize"
	siwcTokenURL     = siwcIssuer + "/api/accounts/oauth/token"
	siwcJWKSURL      = siwcIssuer + "/.well-known/jwks.json"
	siwcRevokeURL    = siwcIssuer + "/api/accounts/oauth/revoke"
	siwcResource     = "https://api.openai.com/v1"
	siwcScope        = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	siwcHostIDKey    = "siwc_ext_agent_host_id"
	siwcDynamicID    = "dynamic_agent_client"
	siwcPendingTTL   = 10 * time.Minute
)

type siwcPending struct {
	ProfileID   int64
	State       string
	Nonce       string
	Verifier    string
	RedirectURI string
	ClientID    string
	NewClient   bool
	StartedAt   time.Time
	Processing  bool
	Done        bool
	Err         string
}

type siwcTokenResponse struct {
	AccessToken       string `json:"access_token"`
	RefreshToken      string `json:"refresh_token"`
	IDToken           string `json:"id_token"`
	TokenType         string `json:"token_type"`
	ExpiresIn         int64  `json:"expires_in"`
	Scope             string `json:"scope"`
	EarliestRefreshAt int64  `json:"earliest_refresh_at"`
	Error             string `json:"error"`
	ErrorDescription  string `json:"error_description"`
}

func randomBase64URL(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newHostID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("urn:uuid:%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func (s *Server) siwcHostID() (string, error) {
	if v, ok, err := s.m.pg.GetSetting(siwcHostIDKey); err != nil {
		return "", err
	} else if ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	v, err := newHostID()
	if err != nil {
		return "", err
	}
	if err := s.m.pg.SetSetting(siwcHostIDKey, v); err != nil {
		return "", err
	}
	return v, nil
}

func siwcRedirectURI(r *http.Request) (string, error) {
	if raw := strings.TrimSpace(os.Getenv("ARTEX_SIWC_REDIRECT_URI")); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/api/llm/oauth/callback" {
			return "", fmt.Errorf("ARTEX_SIWC_REDIRECT_URI는 http://127.0.0.1:<port>/api/llm/oauth/callback 형식이어야 합니다")
		}
		return u.String(), nil
	}
	port := "8787"
	if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "" {
		port = p
	}
	return "http://127.0.0.1:" + port + "/api/llm/oauth/callback", nil
}

func (s *Server) oauthEncrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	key := sha256.Sum256(append(append([]byte{}, s.jwtKey...), []byte("\x00artex-siwc-v1")...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), []byte("artex-siwc-token"))
	return "v1." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *Server) oauthDecrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if !strings.HasPrefix(ciphertext, "v1.") {
		return "", fmt.Errorf("지원하지 않는 OAuth 자격 증명 형식")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ciphertext, "v1."))
	if err != nil {
		return "", err
	}
	key := sha256.Sum256(append(append([]byte{}, s.jwtKey...), []byte("\x00artex-siwc-v1")...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("손상된 OAuth 자격 증명")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte("artex-siwc-token"))
	if err != nil {
		return "", fmt.Errorf("OAuth 자격 증명을 복호화할 수 없습니다")
	}
	return string(plain), nil
}

func (s *Server) startLLMOAuth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileID int64 `json:"profile_id"`
	}
	if err := decode(r, &body); err != nil || body.ProfileID <= 0 {
		writeErr(w, http.StatusBadRequest, "모델 설정 id가 필요합니다")
		return
	}
	p, err := s.m.pg.ProfileByID(body.ProfileID)
	if err != nil || p == nil {
		writeErr(w, http.StatusNotFound, "모델 설정을 찾을 수 없습니다")
		return
	}
	if p.AuthType != db.LLMAuthChatGPTOAuth {
		writeErr(w, http.StatusBadRequest, "ChatGPT OAuth 인증으로 저장한 모델 설정만 연결할 수 있습니다")
		return
	}
	hostID, err := s.siwcHostID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	redirectURI, err := siwcRedirectURI(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	state, err := randomBase64URL(32)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	nonce, err := randomBase64URL(32)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	verifier, err := randomBase64URL(48)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	challengeRaw := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeRaw[:])

	clientID := strings.TrimSpace(p.OAuthClientID)
	newClient := clientID == ""
	if newClient {
		clientID = siwcDynamicID
	}
	q := url.Values{
		"client_id":             {clientID},
		"ext_agent_host_id":     {hostID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {siwcScope},
		"resource":              {siwcResource},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge_method": {"S256"},
		"code_challenge":        {challenge},
	}
	if newClient {
		q.Set("agent_name_hint", "ARTEX")
	} else {
		if hint, derr := s.oauthDecrypt(p.OAuthIDToken); derr == nil && hint != "" {
			q.Set("id_token_hint", hint)
		}
		if p.OAuthEmail != "" {
			q.Set("login_hint", p.OAuthEmail)
		}
	}

	s.oauthStateMu.Lock()
	for k, v := range s.oauthPending {
		if time.Since(v.StartedAt) > siwcPendingTTL {
			delete(s.oauthPending, k)
		}
	}
	s.oauthPending[state] = &siwcPending{
		ProfileID: body.ProfileID, State: state, Nonce: nonce, Verifier: verifier,
		RedirectURI: redirectURI, ClientID: clientID, NewClient: newClient, StartedAt: time.Now(),
	}
	s.oauthStateMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "authorization_url": siwcAuthorizeURL + "?" + q.Encode()})
}

func (s *Server) llmOAuthStatus(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	s.oauthStateMu.Lock()
	p := s.oauthPending[state]
	if p == nil || time.Since(p.StartedAt) > siwcPendingTTL {
		s.oauthStateMu.Unlock()
		writeErr(w, http.StatusNotFound, "로그인 시도가 만료되었거나 존재하지 않습니다")
		return
	}
	out := map[string]any{"done": p.Done, "error": p.Err, "profile_id": p.ProfileID}
	if p.Done {
		delete(s.oauthPending, state)
	}
	s.oauthStateMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) finishOAuthPending(state, message string) {
	s.oauthStateMu.Lock()
	if p := s.oauthPending[state]; p != nil {
		p.Done = true
		p.Err = message
	}
	s.oauthStateMu.Unlock()
}

func oauthCallbackHTML(w http.ResponseWriter, ok bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	status := "연결되었습니다"
	if !ok {
		status = "연결하지 못했습니다"
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="ko"><meta charset="utf-8"><title>ARTEX ChatGPT 연결</title><body style="font-family:system-ui;padding:32px"><h2>%s</h2><p>%s</p><script>try{window.opener&&window.opener.postMessage({type:"artex-siwc",ok:%t},window.location.origin)}catch(e){};setTimeout(()=>window.close(),900)</script></body></html>`, html.EscapeString(status), html.EscapeString(message), ok)
}

func (s *Server) llmOAuthCallback(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	s.oauthStateMu.Lock()
	pending := s.oauthPending[state]
	if pending == nil || state == "" || time.Since(pending.StartedAt) > siwcPendingTTL {
		s.oauthStateMu.Unlock()
		oauthCallbackHTML(w, false, "로그인 상태가 만료되었습니다. ARTEX에서 다시 시도해 주세요.")
		return
	}
	// Consume the callback attempt before exchanging its one-time code. This
	// prevents concurrent/replayed callback requests from rotating and saving the
	// same credential set twice. A failed attempt is intentionally terminal; the
	// user starts a fresh PKCE flow from the profile editor.
	if pending.Processing || pending.Done {
		s.oauthStateMu.Unlock()
		oauthCallbackHTML(w, false, "이미 처리 중이거나 완료된 로그인 요청입니다.")
		return
	}
	pending.Processing = true
	s.oauthStateMu.Unlock()
	if oauthErr := strings.TrimSpace(r.URL.Query().Get("error")); oauthErr != "" {
		msg := oauthErr
		if d := strings.TrimSpace(r.URL.Query().Get("error_description")); d != "" {
			msg += ": " + d
		}
		s.finishOAuthPending(state, msg)
		oauthCallbackHTML(w, false, msg)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	clientID := pending.ClientID
	callbackClientID := strings.TrimSpace(r.URL.Query().Get("client_id"))
	if pending.NewClient {
		if callbackClientID == "" || callbackClientID == siwcDynamicID {
			s.finishOAuthPending(state, "발급된 OAuth client_id가 없습니다")
			oauthCallbackHTML(w, false, "OpenAI가 발급한 client_id를 받지 못했습니다.")
			return
		}
		clientID = callbackClientID
	} else if callbackClientID != "" && callbackClientID != clientID {
		s.finishOAuthPending(state, "OAuth client_id가 기존 등록과 일치하지 않습니다")
		oauthCallbackHTML(w, false, "기존 ChatGPT 등록과 다른 client_id가 반환되었습니다.")
		return
	}
	if code == "" {
		s.finishOAuthPending(state, "authorization code가 없습니다")
		oauthCallbackHTML(w, false, "인증 코드가 반환되지 않았습니다.")
		return
	}
	tokens, err := exchangeSIWCToken(r.Context(), url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {pending.Verifier},
		"redirect_uri":  {pending.RedirectURI},
		"resource":      {siwcResource},
	})
	if err != nil {
		s.finishOAuthPending(state, err.Error())
		oauthCallbackHTML(w, false, err.Error())
		return
	}
	if !scopeHas(tokens.Scope, "chatgpt.tokens.use.direct") {
		s.finishOAuthPending(state, "ChatGPT 플랜 사용 권한이 승인되지 않았습니다")
		oauthCallbackHTML(w, false, "ChatGPT 플랜 사용 권한이 승인되지 않았습니다.")
		return
	}
	identity, err := validateSIWCIDToken(r.Context(), tokens.IDToken, clientID, pending.Nonce)
	if err != nil {
		s.finishOAuthPending(state, err.Error())
		oauthCallbackHTML(w, false, "ID 토큰을 검증하지 못했습니다: "+err.Error())
		return
	}
	profile, _ := s.m.pg.ProfileByID(pending.ProfileID)
	if profile == nil {
		s.finishOAuthPending(state, "모델 설정이 삭제되었습니다")
		oauthCallbackHTML(w, false, "연결할 모델 설정이 삭제되었습니다.")
		return
	}
	if profile.OAuthSubject != "" && profile.OAuthSubject != identity.Subject {
		s.finishOAuthPending(state, "선택한 ChatGPT 계정이 기존 등록과 다릅니다")
		oauthCallbackHTML(w, false, "기존 등록과 다른 ChatGPT 계정입니다. 새 설정을 만들어 연결해 주세요.")
		return
	}
	if err := s.saveOAuthTokens(profile.ID, identity.Email, identity.Subject, clientID, tokens, profile.OAuthIDToken); err != nil {
		s.finishOAuthPending(state, err.Error())
		oauthCallbackHTML(w, false, "자격 증명을 저장하지 못했습니다.")
		return
	}
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
	s.finishOAuthPending(state, "")
	oauthCallbackHTML(w, true, "ChatGPT 계정 "+identity.Email+"을 ARTEX 모델 설정에 연결했습니다.")
}

func exchangeSIWCToken(ctx context.Context, form url.Values) (*siwcTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, siwcTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenAI 토큰 요청 실패: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out siwcTokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("OpenAI 토큰 응답 해석 실패 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" || out.RefreshToken == "" {
		msg := out.Error
		if out.ErrorDescription != "" {
			msg += ": " + out.ErrorDescription
		}
		if strings.TrimSpace(msg) == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("OpenAI 토큰 요청 실패: %s", msg)
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	return &out, nil
}

type siwcIdentity struct {
	Subject string
	Email   string
}

type siwcJWKS struct {
	Keys []struct {
		KID string `json:"kid"`
		KTY string `json:"kty"`
		ALG string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func validateSIWCIDToken(ctx context.Context, raw, clientID, nonce string) (*siwcIdentity, error) {
	if raw == "" {
		return nil, fmt.Errorf("ID 토큰이 없습니다")
	}
	unverified, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("ID 토큰 헤더 해석 실패")
	}
	kid, _ := unverified.Header["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("ID 토큰 kid가 없습니다")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, siwcJWKSURL, nil)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenAI JWKS 요청 실패: %w", err)
	}
	defer resp.Body.Close()
	var set siwcJWKS
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set) != nil {
		return nil, fmt.Errorf("OpenAI JWKS를 읽지 못했습니다")
	}
	var pub *rsa.PublicKey
	for _, k := range set.Keys {
		if k.KID != kid || k.KTY != "RSA" || k.ALG != "RS256" {
			continue
		}
		nBytes, nerr := base64.RawURLEncoding.DecodeString(k.N)
		eBytes, eerr := base64.RawURLEncoding.DecodeString(k.E)
		if nerr != nil || eerr != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			continue
		}
		var padded [4]byte
		copy(padded[4-len(eBytes):], eBytes)
		pub = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(binary.BigEndian.Uint32(padded[:]))}
		break
	}
	if pub == nil {
		return nil, fmt.Errorf("ID 토큰 서명 키를 찾을 수 없습니다")
	}
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return pub, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(siwcIssuer),
		jwt.WithAudience(clientID), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid {
		return nil, fmt.Errorf("ID 토큰 서명 또는 필수 claim이 올바르지 않습니다")
	}
	if got, _ := claims["nonce"].(string); got == "" || got != nonce {
		return nil, fmt.Errorf("ID 토큰 nonce가 일치하지 않습니다")
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	if sub == "" {
		return nil, fmt.Errorf("ID 토큰 subject가 없습니다")
	}
	return &siwcIdentity{Subject: sub, Email: email}, nil
}

func scopeHas(scope, wanted string) bool {
	for _, s := range strings.Fields(scope) {
		if s == wanted {
			return true
		}
	}
	return false
}

func (s *Server) saveOAuthTokens(profileID int64, email, subject, clientID string, tokens *siwcTokenResponse, oldEncryptedIDToken string) error {
	idToken := tokens.IDToken
	if idToken == "" && oldEncryptedIDToken != "" {
		var err error
		idToken, err = s.oauthDecrypt(oldEncryptedIDToken)
		if err != nil {
			return err
		}
	}
	access, err := s.oauthEncrypt(tokens.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := s.oauthEncrypt(tokens.RefreshToken)
	if err != nil {
		return err
	}
	idEnc, err := s.oauthEncrypt(idToken)
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix()
	return s.m.pg.SaveProfileOAuth(profileID, email, subject, clientID, access, refresh, idEnc, tokens.Scope, expiresAt)
}

func (s *Server) chatGPTOAuthToken(ctx context.Context, profileID int64) (string, error) {
	s.oauthRefreshMu.Lock()
	defer s.oauthRefreshMu.Unlock()
	p, err := s.m.pg.ProfileByID(profileID)
	if err != nil || p == nil || p.AuthType != db.LLMAuthChatGPTOAuth || !p.OAuthConnected {
		return "", fmt.Errorf("ChatGPT OAuth 연결이 없습니다")
	}
	access, err := s.oauthDecrypt(p.OAuthAccessToken)
	if err != nil {
		return "", err
	}
	if access != "" && time.Now().Unix() < p.OAuthExpiresAt-60 {
		return access, nil
	}
	refresh, err := s.oauthDecrypt(p.OAuthRefreshToken)
	if err != nil || refresh == "" {
		return "", fmt.Errorf("ChatGPT OAuth 갱신 토큰을 사용할 수 없습니다")
	}
	tokens, err := exchangeSIWCToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {p.OAuthClientID},
		"refresh_token": {refresh},
		"resource":      {siwcResource},
	})
	if err != nil {
		return "", err
	}
	if tokens.Scope == "" {
		tokens.Scope = p.OAuthScope
	}
	if !scopeHas(tokens.Scope, "chatgpt.tokens.use.direct") {
		return "", fmt.Errorf("갱신된 토큰에 ChatGPT 플랜 사용 권한이 없습니다")
	}
	if err := s.saveOAuthTokens(profileID, p.OAuthEmail, p.OAuthSubject, p.OAuthClientID, tokens, p.OAuthIDToken); err != nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

func (s *Server) disconnectLLMOAuth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileID int64 `json:"profile_id"`
	}
	if err := decode(r, &body); err != nil || body.ProfileID <= 0 {
		writeErr(w, 400, "모델 설정 id가 필요합니다")
		return
	}
	p, err := s.m.pg.ProfileByID(body.ProfileID)
	if err != nil || p == nil {
		writeErr(w, 404, "모델 설정을 찾을 수 없습니다")
		return
	}
	refresh, _ := s.oauthDecrypt(p.OAuthRefreshToken)
	if refresh != "" && p.OAuthClientID != "" {
		form := url.Values{"token": {refresh}, "token_type_hint": {"refresh_token"}, "client_id": {p.OAuthClientID}}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, siwcRevokeURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if resp, reqErr := (&http.Client{Timeout: 15 * time.Second}).Do(req); reqErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				writeErr(w, 502, "OpenAI 세션 해제에 실패했습니다: HTTP "+strconv.Itoa(resp.StatusCode))
				return
			}
		} else {
			writeErr(w, 502, "OpenAI 세션 해제에 실패했습니다: "+reqErr.Error())
			return
		}
	}
	if err := s.m.pg.ClearProfileOAuth(body.ProfileID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
	writeJSON(w, 200, map[string]any{"ok": true})
}
