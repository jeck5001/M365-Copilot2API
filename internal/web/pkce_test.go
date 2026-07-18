package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestStartPKCEUsesBrowserClientDefaults(t *testing.T) {
	t.Setenv("M365_CLIENT_ID", "")
	t.Setenv("M365_AUTHORITY", "")
	t.Setenv("M365_REDIRECT_URI", "")

	s := &Server{pkce: map[string]pendingPKCE{}}
	rr := httptest.NewRecorder()
	s.startPKCE(rr, httptest.NewRequest(http.MethodPost, "/api/auth/start", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response struct {
		State       string `json:"state"`
		URL         string `json:"url"`
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.State == "" {
		t.Fatal("response omitted state")
	}
	if got, want := response.RedirectURI, "https://login.microsoftonline.com/common/oauth2/nativeclient"; got != want {
		t.Fatalf("redirect URI = %q, want %q", got, want)
	}
	u, err := url.Parse(response.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Query().Get("client_id"), "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"; got != want {
		t.Fatalf("client_id = %q, want %q", got, want)
	}
	if got := u.Query().Get("redirect_uri"); got != response.RedirectURI {
		t.Fatalf("authorization redirect URI = %q, response redirect URI = %q", got, response.RedirectURI)
	}
}

func TestPKCEStatusReportsPendingAndExpired(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"pending": {Created: time.Now(), Status: "pending"},
		"expired": {Created: time.Now().Add(-11 * time.Minute), Status: "pending"},
	}}

	for _, tc := range []struct {
		state string
		want  string
	}{
		{state: "pending", want: "pending"},
		{state: "expired", want: "expired"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.pkceStatus(rr, httptest.NewRequest(http.MethodGet, "/api/auth/status?state="+tc.state, nil))
			var response map[string]any
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if got := response["status"]; got != tc.want {
				t.Fatalf("status = %v, want %q", got, tc.want)
			}
		})
	}
}
