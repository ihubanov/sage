package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOAuth_Register_RejectsBadRedirects confirms /oauth/register refuses
// non-https redirects, userinfo, fragments, and empty input.
func TestOAuth_Register_RejectsBadRedirects(t *testing.T) {
	_, r, _ := newOAuthRouter(t, true, "")
	cases := []map[string]any{
		{"redirect_uris": []string{}},                                   // empty
		{"redirect_uris": []string{"http://chat.openai.com/cb"}},        // http
		{"redirect_uris": []string{"https://user:pass@chat.openai/cb"}}, // userinfo
		{"redirect_uris": []string{"https://chat.openai.com/cb#x"}},     // fragment
		{"redirect_uris": []string{"not-a-url"}},                        // not absolute
	}
	for _, body := range cases {
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(string(buf)))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%+v -> %s", body, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "invalid_redirect_uri")
	}
}

// TestOAuth_Authorize_RejectsUnregisteredClient is the C1 fix: an attacker
// cannot drive /oauth/authorize with a fabricated client_id.
func TestOAuth_Authorize_RejectsUnregisteredClient(t *testing.T) {
	_, r, _ := newOAuthRouter(t, true, "")
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?client_id=never-registered&redirect_uri=https://x/cb&code_challenge=abc&code_challenge_method=S256&response_type=code&state=s",
		nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "client_id is not registered")
}

// TestOAuth_Authorize_RejectsUnregisteredRedirect is the open-redirect close.
// A registered client trying to redirect to an attacker URL must be rejected.
func TestOAuth_Authorize_RejectsUnregisteredRedirect(t *testing.T) {
	_, r, _ := newOAuthRouter(t, true, "")
	clientID := registerOAuthClient(t, r, "https://chat.openai.com/cb")

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?client_id="+url.QueryEscape(clientID)+
			"&redirect_uri=https://attacker.example/grab&code_challenge=abc&code_challenge_method=S256&response_type=code&state=s",
		nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "redirect_uri does not match")
}

// TestOAuth_Authorize_StateMandatory confirms missing state -> 400.
func TestOAuth_Authorize_StateMandatory(t *testing.T) {
	_, r, _ := newOAuthRouter(t, true, "")
	clientID := registerOAuthClient(t, r, "https://x/cb")

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?client_id="+url.QueryEscape(clientID)+
			"&redirect_uri=https://x/cb&code_challenge=abc&code_challenge_method=S256&response_type=code",
		nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "state is required")
}

// TestOAuth_Authorize_POST_RejectsMissingCSRFNonce confirms a POST without a
// signed nonce re-renders the consent screen with an error rather than
// minting a code.
func TestOAuth_Authorize_POST_RejectsMissingCSRFNonce(t *testing.T) {
	_, r, _ := newOAuthRouter(t, true, "")
	redirect := "https://chat.openai.com/cb"
	clientID := registerOAuthClient(t, r, redirect)

	authURL := "/oauth/authorize?client_id=" + url.QueryEscape(clientID) +
		"&redirect_uri=" + url.QueryEscape(redirect) +
		"&code_challenge=abc&code_challenge_method=S256&response_type=code&state=csrf-test"

	form := url.Values{}
	form.Set("agent_id", strings.Repeat("a", 64))
	// no csrf_nonce
	req := httptest.NewRequest(http.MethodPost, authURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "should re-render consent, not 302")
	assert.Contains(t, rr.Body.String(), "Consent request could not be verified")
}

// TestOAuth_Authorize_POST_RejectsForgedCSRFNonce confirms a nonce signed
// for a different /authorize request fails verification.
func TestOAuth_Authorize_POST_RejectsForgedCSRFNonce(t *testing.T) {
	_, r, _ := newOAuthRouter(t, true, "")
	redirect := "https://chat.openai.com/cb"
	clientID := registerOAuthClient(t, r, redirect)

	// Pull a nonce for one set of parameters, attempt to use it for a
	// different state value — verifyCSRFNonce must reject.
	nonceURL := "/oauth/authorize?client_id=" + url.QueryEscape(clientID) +
		"&redirect_uri=" + url.QueryEscape(redirect) +
		"&code_challenge=abc&code_challenge_method=S256&response_type=code&state=original"
	nonce := fetchCSRFNonce(t, r, nonceURL)

	postURL := "/oauth/authorize?client_id=" + url.QueryEscape(clientID) +
		"&redirect_uri=" + url.QueryEscape(redirect) +
		"&code_challenge=abc&code_challenge_method=S256&response_type=code&state=DIFFERENT"
	form := url.Values{}
	form.Set("agent_id", strings.Repeat("a", 64))
	form.Set("csrf_nonce", nonce)
	req := httptest.NewRequest(http.MethodPost, postURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "should re-render consent")
	assert.Contains(t, rr.Body.String(), "Consent request could not be verified")
}

// TestOAuth_Authorize_NoAgentRosterPreAuth confirms a tunnel-exposed visitor
// without a dashboard cookie sees only an 8-char hex prefix of the node
// operator's pubkey, never the full identity or roster.
func TestOAuth_Authorize_NoAgentRosterPreAuth(t *testing.T) {
	// HasDashboardCookie is left nil — every request appears unauthenticated
	// to the consent renderer regardless of IsAuthed's verdict.
	_, r, _ := newOAuthRouter(t, true, "")
	clientID := registerOAuthClient(t, r, "https://x/cb")

	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?client_id="+url.QueryEscape(clientID)+
			"&redirect_uri=https://x/cb&code_challenge=abc&code_challenge_method=S256&response_type=code&state=s",
		nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	body := rr.Body.String()
	require.Equal(t, http.StatusOK, rr.Code)
	// No picker / dropdown / free-text input for agent_id.
	assert.NotContains(t, body, `<select name="agent_id"`)
	assert.NotContains(t, body, `name="agent_id"`)
	// The operator label is the 8-char prefix of the test handler's
	// NodeOperatorAgentID (64×'a' → "aaaaaaaa…").
	assert.Contains(t, body, "aaaaaaaa")
	// The full pubkey must NOT be rendered to an unauthenticated visitor.
	assert.NotContains(t, body, strings.Repeat("a", 64))
}

// TestOAuth_Register_RateLimited confirms /oauth/register limits abuse from
// a single remote address.
func TestOAuth_Register_RateLimited(t *testing.T) {
	_, r, _ := newOAuthRouter(t, true, "")
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{"https://chat.openai.com/cb"},
	})
	limited := false
	for i := 0; i < dcrRegisterPerIPLimit+5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.42:1234"
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	assert.True(t, limited, "expected rate-limit response after %d registrations", dcrRegisterPerIPLimit)
}
