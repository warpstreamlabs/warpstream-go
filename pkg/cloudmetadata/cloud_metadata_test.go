package cloudmetadata

import (
	"context"
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
		if r.URL.Path != "/metadata/instance" {
			t.Errorf("Expected to request '/metadata/instance', got: %s", r.URL.Path)
		}
		if r.Header.Get("Metadata") != "true" {
			t.Errorf("Expected Metadata: true header, got: %s", r.Header.Get("Metadata"))
		}

		if r.URL.Query().Get("format") != "json" {
			t.Errorf("Expected format: json query param, got: %s", r.URL.Query().Get("format"))
		}
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("Expected format: json query param, got: %s", r.URL.Query().Get("format"))
		}
		if r.URL.Query().Get("api-version") != "2024-07-17" {
			t.Errorf("Expected api-version: 2024-07-17 query param, got: %s", r.URL.Query().Get("api-version"))
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`
		{
			"compute": {
				"physicalZone": "useast-AZ01"
			}
		}
		`))
	}))
	defer server.Close()

	ctx := context.TODO()
	ctx = context.WithValue(ctx, "url", server.URL)
	zone, err := availabilityZoneAzure(ctx)
	require.NoError(t, err)
	require.Equal(t, zone, "useast-AZ01")
}
