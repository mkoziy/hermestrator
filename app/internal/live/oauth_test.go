package live

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFormXSRFBridgePromotesCookieForRegularFormPosts(t *testing.T) {
	handler := formXSRFBridge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-XSRF-TOKEN"); got != "form-token" {
			t.Fatalf("XSRF header = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/repositories/42", nil)
	req.AddCookie(&http.Cookie{Name: "XSRF-TOKEN", Value: "form-token"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestFormXSRFBridgeDoesNotOverrideHTMXHeader(t *testing.T) {
	handler := formXSRFBridge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-XSRF-TOKEN"); got != "htmx-token" {
			t.Fatalf("XSRF header = %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodPost, "/repositories/42", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-XSRF-TOKEN", "htmx-token")
	req.AddCookie(&http.Cookie{Name: "XSRF-TOKEN", Value: "form-token"})

	handler.ServeHTTP(httptest.NewRecorder(), req)
}
