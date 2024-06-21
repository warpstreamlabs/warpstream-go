package cloudmetadata

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGCPHandling(t *testing.T) {
	require.Equal(t, "", maybeProcessGCPAZ(""))
	require.Equal(t, "us-central1-a", maybeProcessGCPAZ("projects/XXXXXXXXX/zones/us-central1-a"))
	require.Equal(t, "us-central1-a", maybeProcessGCPAZ("us-central1-a"))
}
