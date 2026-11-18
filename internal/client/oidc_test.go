package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOIDCClientCredentialsLoginAndRefresh(t *testing.T) {
	var tokensIssued int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "sec" {
			t.Errorf("unexpected token request form: %v", r.Form)
		}
		n := atomic.AddInt32(&tokensIssued, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"access_token":"old"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"new"}`))
	}))
	defer idp.Close()

	superset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/security/login" {
			t.Error("OIDC client must not use the Superset login endpoint")
		}
		if r.Header.Get("Authorization") != "Bearer new" {
			w.WriteHeader(http.StatusUnauthorized) // the first token counts as expired
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer superset.Close()

	c := NewClientWithOIDC(superset.URL, idp.URL, "cid", "sec")
	c.CSRFEnabled = false
	resp, err := c.DoRequest("GET", "/api/v1/me/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 after the token was refreshed", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&tokensIssued); got != 2 {
		t.Fatalf("tokens issued %d, want 2 (initial + refresh on 401)", got)
	}
}

func TestOIDCClientMissingSecretIsReported(t *testing.T) {
	c := NewClientWithOIDC("http://superset", "http://idp/token", "cid", "")
	if err := c.Authenticate(); err == nil {
		t.Fatal("expected an error for a missing client_secret")
	}
}
