// MIT License
//
// Copyright (c) 2023 TTBT Enterprises LLC
// Copyright (c) 2023 Robin Thellend <rthellend@rthellend.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package proxy

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/c2FmZQ/tlsproxy/proxy/internal/cookiemanager"
)

type ctxAuthKey struct{}

const (
	xTLSProxyUserIDHeader = "X-tlsproxy-user-id"
)

var (
	authCtxKey ctxAuthKey

	//go:embed permission-denied-template.html
	permissionDeniedEmbed    string
	permissionDeniedTemplate *template.Template
	//go:embed logout-template.html
	logoutEmbed    string
	logoutTemplate *template.Template
	//go:embed sso-status-template.html
	ssoStatusEmbed    string
	ssoStatusTemplate *template.Template
	//go:embed style.css
	styleEmbed []byte
)

func init() {
	permissionDeniedTemplate = template.Must(template.New("permission-denied").Parse(permissionDeniedEmbed))
	logoutTemplate = template.Must(template.New("logout").Parse(logoutEmbed))
	ssoStatusTemplate = template.Must(template.New("sso-status").Parse(ssoStatusEmbed))
}

func claimsFromCtx(ctx context.Context) jwt.MapClaims {
	if v := ctx.Value(authCtxKey); v != nil {
		return v.(jwt.MapClaims)
	}
	return nil
}

func (be *Backend) userAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Del(xTLSProxyUserIDHeader)
		if be.SSO != nil {
			claims, cont := be.checkCookies(w, req)
			if !cont {
				return
			}
			if claims != nil {
				if email, ok := claims["email"].(string); ok && email != "" {
					if be.SSO.SetUserIDHeader {
						req.Header.Set(xTLSProxyUserIDHeader, email)
					}
					req = req.WithContext(context.WithValue(req.Context(), authCtxKey, claims))
				}
			}
		}
		next.ServeHTTP(w, req)
	})
}

func (be *Backend) checkCookies(w http.ResponseWriter, req *http.Request) (jwt.MapClaims, bool) {
	// If a valid ID Token is in the authorization header, use it and
	// ignore the cookies.
	if tok, err := be.SSO.cm.ValidateAuthorizationHeader(req); err == nil {
		return tok.Claims.(jwt.MapClaims), true
	}

	authToken, err := be.SSO.cm.ValidateAuthTokenCookie(req)
	if err != nil {
		return nil, true
	}
	authClaims, ok := authToken.Claims.(jwt.MapClaims)
	if !ok {
		return nil, true
	}
	if email, ok := authClaims["email"].(string); !ok || email == "" {
		return nil, true
	}

	if !be.SSO.GenerateIDTokens {
		return authClaims, true
	}

	if !slices.Contains(be.ServerNames, hostFromReq(req)) {
		return authClaims, true
	}

	if err := be.SSO.cm.ValidateIDTokenCookie(req, authToken); err == nil {
		// Token is already set, and is valid.
		return authClaims, true
	}
	if err := be.SSO.cm.SetIDTokenCookie(w, req, authToken); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	http.Redirect(w, req, req.URL.String(), http.StatusFound)
	return nil, false
}

func (be *Backend) serveSSOStyle(w http.ResponseWriter, req *http.Request) {
	sum := sha256.Sum256(styleEmbed)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`

	w.Header().Set("Content-Type", "text/css")
	w.Header().Set("Cache-Control", "public")
	w.Header().Set("Etag", etag)

	if e := req.Header.Get("If-None-Match"); e == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(styleEmbed)
}

func (be *Backend) serveSSOStatus(w http.ResponseWriter, req *http.Request) {
	claims := claimsFromCtx(req.Context())
	var keys []string
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type kv struct {
		Key, Value string
	}
	var data struct {
		Token  string
		Claims []kv
	}
	for _, k := range keys {
		if k == "iat" {
			v, _ := claims.GetIssuedAt()
			data.Claims = append(data.Claims, kv{k, v.String()})
			continue
		}
		if k == "exp" {
			v, _ := claims.GetExpirationTime()
			data.Claims = append(data.Claims, kv{k, v.String()})
			continue
		}
		data.Claims = append(data.Claims, kv{k, fmt.Sprint(claims[k])})
	}
	token, _, err := be.makeTokenForURL(req)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.Token = token
	ssoStatusTemplate.Execute(w, data)
}

func (be *Backend) serveLogout(w http.ResponseWriter, req *http.Request) {
	if be.SSO != nil {
		be.SSO.cm.ClearCookies(w)
	}
	req.ParseForm()
	if tokenStr := req.Form.Get("u"); tokenStr != "" {
		tok, err := be.tm.ValidateToken(tokenStr)
		if err == jwt.ErrTokenExpired {
			http.Error(w, "data expired", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		c, ok := tok.Claims.(jwt.MapClaims)
		if !ok {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		url, ok := c["url"].(string)
		if !ok {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		be.SSO.p.RequestLogin(w, req, url)
		return
	}
	logoutTemplate.Execute(w, nil)
}

func (be *Backend) servePermissionDenied(w http.ResponseWriter, req *http.Request) {
	var email string
	if claims := claimsFromCtx(req.Context()); claims != nil {
		email, _ = claims["email"].(string)
	}
	token, url, err := be.makeTokenForURL(req)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := struct {
		Email      string
		URL        string
		DisplayURL string
		Token      string
	}{
		Email:      email,
		URL:        url,
		DisplayURL: url,
		Token:      token,
	}
	if len(data.DisplayURL) > 100 {
		data.DisplayURL = data.DisplayURL[:97] + "..."
	}
	w.WriteHeader(http.StatusForbidden)
	permissionDeniedTemplate.Execute(w, data)
}

func (be *Backend) enforceSSOPolicy(w http.ResponseWriter, req *http.Request) bool {
	// Filter out the tlsproxy auth cookie.
	cookiemanager.FilterOutAuthTokenCookie(req)

	if be.SSO == nil || !pathMatches(be.SSO.Paths, req.URL.Path) {
		return true
	}
	claims := claimsFromCtx(req.Context())
	var iat time.Time
	if claims != nil {
		if p, _ := claims.GetIssuedAt(); p != nil {
			iat = p.Time
		}
	}
	hh := sha256.Sum256([]byte(req.Host))
	// Request authentication when:
	// * the user isn't logged in, or
	// * the backend has ForceReAuth set, and the last authentication
	//   either on a different host, or too long ago.
	if claims == nil || (be.SSO.ForceReAuth != 0 && (claims["hhash"] != hex.EncodeToString(hh[:]) || time.Since(iat) > be.SSO.ForceReAuth)) {
		u := req.URL
		u.Scheme = "https"
		u.Host = req.Host
		log.Printf("REQ %s ➔ %s %s ➔ status:%d (SSO) (%q)", formatReqDesc(req), req.Method, req.RequestURI, http.StatusFound, userAgent(req))
		be.SSO.p.RequestLogin(w, req, u.String())
		return false
	}
	userID, _ := claims["email"].(string)
	host := hostFromReq(req)
	_, userDomain, _ := strings.Cut(userID, "@")
	if be.SSO.ACL != nil && !slices.Contains(*be.SSO.ACL, userID) && !slices.Contains(*be.SSO.ACL, "@"+userDomain) {
		be.recordEvent(fmt.Sprintf("deny SSO %s to %s", userID, host))
		log.Printf("REQ %s ➔ %s %s ➔ status:%d (SSO) (%q)", formatReqDesc(req), req.Method, req.RequestURI, http.StatusForbidden, userAgent(req))
		be.servePermissionDenied(w, req)
		return false
	}
	be.recordEvent(fmt.Sprintf("allow SSO %s to %s", userID, host))
	return true
}

func pathMatches(prefixes []string, path string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (be *Backend) makeTokenForURL(req *http.Request) (string, string, error) {
	u := req.URL
	u.Scheme = "https"
	u.Host = req.Host
	token, err := be.tm.CreateToken(jwt.MapClaims{
		"url": u.String(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}, "EdDSA")
	return token, u.String(), err
}
