package repl

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A forwarded publish carries the headers that describe its payload.
//
// It is not decoration: a NuGet push is a multipart body, and a home site
// that receives it without its Content-Type cannot see the package inside
// the envelope — it stores the envelope, and the client that pushed it gets
// back an archive its own tooling refuses. That is exactly what happened
// before this, and nothing failed loudly: the upload was accepted, the
// version was listed, and only `dotnet restore` on the other side noticed.
func TestAForwardedPublishCarriesThePayloadsHeaders(t *testing.T) {
	var seen ForwardedPublish
	var body []byte

	server := NewServer(ServerOptions{
		Site:      "home",
		Authorize: func(*http.Request) (string, error) { return "peer", nil },
		Publish: func(_ context.Context, req ForwardedPublish) (int, http.Header, []byte, error) {
			seen = req
			body, _ = io.ReadAll(req.Body)
			return http.StatusCreated, http.Header{"Location": []string{"/somewhere"}}, []byte("ok"), nil
		},
	})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := NewClient(Peer{Name: "home", URL: ts.URL}, ts.Client(), nil, nil)
	header := http.Header{}
	header.Set("Content-Type", "multipart/form-data; boundary=abc123")
	header.Set("Content-Range", "0-41")

	status, respHeader, respBody, err := client.ForwardPublish(context.Background(),
		"nuget-hosted", "api/v2/package/", http.MethodPut,
		strings.NewReader("--abc123\r\nthe package\r\n--abc123--"), header,
		"token:ci", "group/project")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if status != http.StatusCreated || string(respBody) != "ok" {
		t.Fatalf("status %d body %q", status, respBody)
	}
	// The home site's own answer comes back whole: a protocol whose next
	// request is named in a header cannot be continued otherwise.
	if respHeader.Get("Location") != "/somewhere" {
		t.Errorf("the home site's Location header did not survive: %q", respHeader.Get("Location"))
	}

	if got := seen.Header.Get("Content-Type"); got != "multipart/form-data; boundary=abc123" {
		t.Errorf("Content-Type arrived as %q", got)
	}
	if got := seen.Header.Get("Content-Range"); got != "0-41" {
		t.Errorf("Content-Range arrived as %q", got)
	}
	if seen.Identity != "token:ci" || seen.ProjectPath != "group/project" {
		t.Errorf("on-behalf-of identity arrived as %q/%q", seen.Identity, seen.ProjectPath)
	}
	if !strings.Contains(string(body), "the package") {
		t.Errorf("body arrived as %q", body)
	}
}
