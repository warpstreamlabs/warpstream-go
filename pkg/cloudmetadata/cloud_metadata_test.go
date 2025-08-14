package cloudmetadata

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGCPHandling(t *testing.T) {
	require.Equal(t, "", maybeProcessGCPAZ(""))
	require.Equal(t, "us-central1-a", maybeProcessGCPAZ("projects/XXXXXXXXX/zones/us-central1-a"))
	require.Equal(t, "us-central1-a", maybeProcessGCPAZ("us-central1-a"))
}

func TestAzureZone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			t.Errorf("Expected Metadata: true header, got: %s", r.Header.Get("Metadata"))
		}

		if r.URL.Query().Get("format") != "text" {
			t.Errorf("Expected format: text query param, got: %s", r.URL.Query().Get("format"))
		}
		if r.URL.Query().Get("api-version") != "2024-07-17" {
			t.Errorf("Expected api-version: 2024-07-17 query param, got: %s", r.URL.Query().Get("api-version"))
		}

		if r.URL.Path == "/metadata/instance/compute/location" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`eastus`))
		} else if r.URL.Path == "/metadata/instance/compute/zone" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`1`))
		} else {
			t.Errorf("Unknown metadata path', got: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	ctx := context.TODO()
	ctx = context.WithValue(ctx, "url", server.URL)
	zone, err := availabilityZoneAzure(ctx, slog.Default())
	require.NoError(t, err)
	require.Equal(t, "eastus-1", zone)
}
