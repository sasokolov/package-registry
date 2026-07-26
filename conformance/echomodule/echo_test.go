package echomodule

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func TestParse(t *testing.T) {
	m := Module{}

	intent, err := m.Parse(httptest.NewRequest("GET", "/data/hello.txt", nil))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentArtifact || intent.RemotePath != "data/hello.txt" {
		t.Errorf("artifact intent = %+v", intent)
	}

	intent, err = m.Parse(httptest.NewRequest("GET", "/meta/info.json", nil))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentMetadata || intent.CacheTTL != MetadataTTL {
		t.Errorf("metadata intent = %+v", intent)
	}

	if _, err := m.Parse(httptest.NewRequest("GET", "/", nil)); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("empty path err = %v, want ErrNotFound", err)
	}
}
