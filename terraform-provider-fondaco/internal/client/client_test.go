package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These run without a registry. What they check is the part of the client a
// resource depends on being exactly right: which failures are "gone", which
// are "someone else wrote first", and whether the credential is actually
// attached to the request.

func TestNewRejectsSomethingThatIsNotAnEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "registry.example.com", "ftp://registry", "://"} {
		if _, err := New(Options{Endpoint: endpoint}); err == nil {
			t.Errorf("endpoint %q was accepted", endpoint)
		}
	}
}

func TestRequestsCarryTheCredentialAndTheAPIPath(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"site":"eu"}`))
	}))
	defer server.Close()

	c, err := New(Options{Endpoint: server.URL, Token: "reg_secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var site Site
	if err := c.Get(context.Background(), "/status", &site); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "Bearer reg_secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != APIPath+"/status" {
		t.Errorf("path = %q, want %q", gotPath, APIPath+"/status")
	}
	if site.Site != "eu" {
		t.Errorf("site = %q", site.Site)
	}
}

func TestStatusesBecomeTheErrorsResourcesActOn(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantGone   bool
		wantStatus int
		wantText   string
	}{
		{
			name:   "a missing resource is gone, not an error to report",
			status: http.StatusNotFound, body: `{"error":"no feed named x"}`,
			wantGone: true,
		},
		{
			name:   "a conflict keeps its status so the caller can say why",
			status: http.StatusConflict, body: `{"error":"the configuration changed"}`,
			wantStatus: http.StatusConflict, wantText: "the configuration changed",
		},
		{
			name:   "a rejected document is reported as invalid",
			status: 422, body: `{"error":"feeds[0]: name is required"}`,
			wantStatus: 422, wantText: "name is required",
		},
		{
			name:   "a body that is not JSON is still shown to the operator",
			status: http.StatusBadGateway, body: "upstream said no",
			wantStatus: http.StatusBadGateway, wantText: "upstream said no",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			c, err := New(Options{Endpoint: server.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = c.Get(context.Background(), "/config/feeds/x", &Feed{})

			if tc.wantGone {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("err = %v, want ErrNotFound", err)
				}
				return
			}
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *client.Error", err)
			}
			if apiErr.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", apiErr.Status, tc.wantStatus)
			}
			if !strings.Contains(apiErr.Message, tc.wantText) {
				t.Errorf("message = %q, want it to contain %q", apiErr.Message, tc.wantText)
			}
			if tc.wantStatus == http.StatusConflict && !apiErr.Conflict() {
				t.Error("a 409 does not report itself as a conflict")
			}
			if tc.wantStatus == 422 && !apiErr.Invalid() {
				t.Error("a 422 does not report itself as invalid")
			}
		})
	}
}

func TestPutSendsTheBodyAndReturnsTheVersion(t *testing.T) {
	var got Feed
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"version":"abc123","created":true}`))
	}))
	defer server.Close()

	c, err := New(Options{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := c.Put(context.Background(), "/config/feeds/eu", Feed{
		Name: "eu", Format: "maven", Hosted: true, Publishers: []string{"token:ci-*"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !result.Created || result.Version != "abc123" {
		t.Fatalf("result = %+v", result)
	}
	if got.Name != "eu" || !got.Hosted || len(got.Publishers) != 1 {
		t.Fatalf("the registry received %+v", got)
	}
}

// Identities that are not URL-safe — an issuer URL, an admin pattern with a
// slash — have to survive the trip intact.
func TestQueryEncodesIdentitiesThatArePathHostile(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("pattern")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	c, err := New(Options{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const pattern = "project:group/sub*"
	if err := c.Get(context.Background(), Query("/config/admins/binding", "pattern", pattern), nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != pattern {
		t.Fatalf("the registry saw %q, want %q", got, pattern)
	}
}
