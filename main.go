package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"
)

// ---- config from env ----
var (
	listenAddr   = env("LISTEN_ADDR", ":8088")
	upstream     = env("LAKEFS_UPSTREAM", "http://lakefs:8000")     // lakeFS to proxy to
	sharedSecret = []byte(os.Getenv("LAKEFS_SHARED_SECRET"))        // == LAKEFS_AUTH_ENCRYPT_SECRET_KEY
	aclBase      = env("ACL_BASE", "http://lakefs-aclserver:8001/api/v1/auth")
	issuerURL    = os.Getenv("OIDC_ISSUER")                          // https://sso.orthant.ai/application/o/lakefs/
	clientID     = os.Getenv("OIDC_CLIENT_ID")
	clientSecret = os.Getenv("OIDC_CLIENT_SECRET")
	redirectURL  = os.Getenv("OIDC_REDIRECT_URL")                    // http://10.0.1.2:8088/oidc/callback
	groupsClaim  = env("OIDC_GROUPS_CLAIM", "groups")
	userClaim    = env("OIDC_USERNAME_CLAIM", "preferred_username")
	defaultGroup = env("LAKEFS_DEFAULT_GROUP", "Readers")            // fallback group if no IdP group maps to a lakeFS group
	// GROUP_MAP: explicit "idpGroup:lakefsGroup" pairs, comma-separated, e.g.
	//   GROUP_MAP=Lakefs-user:Lakefs-user,lakefs-admins:Lakefs-admin
	// When set, ONLY listed IdP groups are honored (names may differ on each side);
	// any IdP group not in the map grants nothing. When empty, falls back to same-name matching.
	groupMap = parseGroupMap(os.Getenv("GROUP_MAP"))
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func parseGroupMap(s string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if i := strings.LastIndex(pair, ":"); i > 0 {
			m[strings.TrimSpace(pair[:i])] = strings.TrimSpace(pair[i+1:])
		}
	}
	return m
}

// mapAuthGroups translates IdP group names to lakeFS group names.
// With GROUP_MAP set: only mapped groups pass through (renamed); others dropped.
// Without it: identity (same-name) mapping.
func mapAuthGroups(authGroups []string) []string {
	out := []string{}
	for _, g := range authGroups {
		if len(groupMap) > 0 {
			if lk, ok := groupMap[g]; ok {
				out = append(out, lk)
			}
		} else {
			out = append(out, g)
		}
	}
	return out
}

var (
	store    = sessions.NewCookieStore(sharedSecret) // SAME encoding as lakeFS internal_auth_session
	oauthCfg oauth2.Config
	verifier *oidc.IDTokenVerifier
	// transient login-state cookie (state+nonce); signed with shared secret too
	stateStore = sessions.NewCookieStore(sharedSecret)
)

func main() {
	if len(sharedSecret) == 0 || issuerURL == "" || clientID == "" {
		log.Fatal("missing required env: LAKEFS_SHARED_SECRET / OIDC_ISSUER / OIDC_CLIENT_ID")
	}
	ctx := context.Background()
	// issuer must EXACTLY match the discovery doc's "issuer" (Authentik keeps a trailing slash)
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		log.Fatalf("oidc discovery: %v", err)
	}
	verifier = provider.Verifier(&oidc.Config{ClientID: clientID})
	oauthCfg = oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", groupsClaim},
	}

	up, _ := url.Parse(upstream)
	proxy := httputil.NewSingleHostReverseProxy(up)

	mux := http.NewServeMux()
	mux.HandleFunc("/_shim/login", handleLogin)
	mux.HandleFunc("/oidc/callback", handleCallback)
	mux.HandleFunc("/_shim/logout", handleLogout)
	mux.HandleFunc("/_shim/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	// optional: SCIM 2.0 server for IdP-pushed lifecycle (real-time deprovisioning)
	if scimEnabled() {
		registerSCIM(mux)
	}
	// everything else proxied to lakeFS
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// seamless UX: a browser navigation with no session -> straight to SSO.
		// lakeFS is a SPA (it client-side-routes to /auth/login, which never hits the
		// server), so we key off "is this a top-level HTML GET without our cookie?".
		// XHR/fetch (Accept */*), static assets, API and S3-gateway/access-key calls
		// don't send Accept: text/html, so they pass through untouched (and get 401 if
		// unauthorized, rather than a redirect).
		if browserNavWithoutSession(r) {
			http.Redirect(w, r, "/_shim/login", http.StatusFound)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	log.Printf("lakefs-sso-shim listening on %s -> %s", listenAddr, upstream)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

// browserNavWithoutSession reports whether this is an unauthenticated top-level
// browser navigation that should be bounced to SSO (vs an asset/XHR/API/S3 call).
func browserNavWithoutSession(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if _, err := r.Cookie("internal_auth_session"); err == nil {
		return false // already has a session cookie
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false // assets send text/css|image/*..., fetch/XHR send */*, S3/CLI send */* — not a page nav
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/_shim") || strings.HasPrefix(p, "/oidc") || strings.HasPrefix(p, "/scim") {
		return false
	}
	return true
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randHex()
	nonce := randHex()
	sess, _ := stateStore.Get(r, "shim_login_state")
	sess.Options = &sessions.Options{Path: "/", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	sess.Values["state"] = state
	sess.Values["nonce"] = nonce
	_ = sess.Save(r, w)
	http.Redirect(w, r, oauthCfg.AuthCodeURL(state, oidc.Nonce(nonce)), http.StatusFound)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess, _ := stateStore.Get(r, "shim_login_state")
	wantState, _ := sess.Values["state"].(string)
	wantNonce, _ := sess.Values["nonce"].(string)
	if r.URL.Query().Get("state") != wantState || wantState == "" {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	oauth2Token, err := oauthCfg.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange: "+err.Error(), http.StatusBadGateway)
		return
	}
	rawID, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	idToken, err := verifier.Verify(ctx, rawID)
	if err != nil {
		http.Error(w, "verify id_token: "+err.Error(), http.StatusBadGateway)
		return
	}
	if idToken.Nonce != wantNonce {
		http.Error(w, "nonce mismatch", http.StatusBadRequest)
		return
	}
	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "claims: "+err.Error(), http.StatusBadGateway)
		return
	}
	username, _ := claims[userClaim].(string)
	if username == "" {
		username, _ = claims["sub"].(string)
	}
	if username == "" {
		http.Error(w, "no username claim", http.StatusBadGateway)
		return
	}
	var groups []string
	if raw, ok := claims[groupsClaim].([]interface{}); ok {
		for _, g := range raw {
			if s, ok := g.(string); ok {
				groups = append(groups, s)
			}
		}
	}

	// provision into lakeFS ACL server
	if err := provision(username, groups); err != nil {
		http.Error(w, "provision: "+err.Error(), http.StatusBadGateway)
		return
	}

	// mint lakeFS login token + set the gorilla cookie lakeFS reads
	tok, err := mintLakeFSToken(username)
	if err != nil {
		http.Error(w, "mint token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	authSess, _ := store.Get(r, "internal_auth_session")
	authSess.Options = &sessions.Options{Path: "/", MaxAge: 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	authSess.Values["token"] = tok
	if err := authSess.Save(r, w); err != nil {
		http.Error(w, "save session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	s, _ := store.Get(r, "internal_auth_session")
	s.Options = &sessions.Options{Path: "/", MaxAge: -1}
	_ = s.Save(r, w)
	http.Redirect(w, r, "/_shim/login", http.StatusFound)
}

func mintLakeFSToken(username string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": "auth", "jti": randHex(), "sub": username,
		"aud": "login", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(sharedSecret)
}

// provision: create user if missing, sync group memberships (same-name mapping to existing lakeFS groups)
func provision(username string, authGroups []string) error {
	// create user (ignore 409 already-exists)
	body, _ := json.Marshal(map[string]string{"username": username})
	if _, err := aclReq("POST", "/users", body, 201, 409); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	// which lakeFS groups exist?
	existing, err := listLakeFSGroups()
	if err != nil {
		return err
	}
	target := map[string]bool{}
	for _, g := range mapAuthGroups(authGroups) {
		if existing[g] {
			target[g] = true
		}
	}
	if len(target) == 0 && defaultGroup != "" && existing[defaultGroup] {
		target[defaultGroup] = true
	}
	// add memberships (idempotent; ignore 201/409)
	for g := range target {
		if _, err := aclReq("PUT", "/groups/"+url.PathEscape(g)+"/members/"+url.PathEscape(username), nil, 201, 409); err != nil {
			log.Printf("warn: add %s to %s: %v", username, g, err)
		}
	}
	log.Printf("provisioned user=%s authGroups=%v -> lakefsGroups=%v", username, authGroups, keys(target))
	return nil
}

func listLakeFSGroups() (map[string]bool, error) {
	resp, err := aclReq("GET", "/groups?prefix=&after=&amount=1000", nil, 200)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, g := range out.Results {
		m[g.Name] = true
	}
	return m, nil
}

func aclReq(method, path string, body []byte, okCodes ...int) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req, _ := http.NewRequest(method, aclBase+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, c := range okCodes {
		if resp.StatusCode == c {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, string(b))
}

func keys(m map[string]bool) []string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	return s
}

func randHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
