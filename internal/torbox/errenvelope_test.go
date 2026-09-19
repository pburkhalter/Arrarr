package torbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TorBox answers a full account with HTTP 500 and an ordinary error envelope.
// Without parsing it the code is lost and the caller sees only the raw JSON as
// Detail, leaving it to substring-match the body to tell backpressure from a
// genuine fault.
func TestAPIErrorCarriesCodeOn500(t *testing.T) {
	const body = `{"success":false,"error":"ACTIVE_LIMIT","detail":"You have reached your active download limit of 10."}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	_, err := c.MyList(context.Background(), true)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%v want *APIError", err)
	}
	if apiErr.Code != "ACTIVE_LIMIT" {
		t.Errorf("Code=%q want ACTIVE_LIMIT (the envelope must be parsed on 5xx too)", apiErr.Code)
	}
	if apiErr.Status != 500 {
		t.Errorf("Status=%d want 500", apiErr.Status)
	}
}

// Responses without an envelope (gateway errors) must still surface something.
func TestAPIErrorFallsBackToRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	_, err := c.MyList(context.Background(), true)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%v want *APIError", err)
	}
	if apiErr.Code != "" {
		t.Errorf("Code=%q want empty", apiErr.Code)
	}
	if apiErr.Detail == "" {
		t.Error("Detail is empty — a bodied gateway error must stay diagnosable")
	}
}
