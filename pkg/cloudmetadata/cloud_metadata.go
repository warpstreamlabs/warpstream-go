package cloudmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	apiv1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
)

const (
	// azRegexPattern represents the regex pattern to match valid availability zone formats.
	// This regex allows for AZ names to be composed of lowercase letters, numbers, and hyphens.
	// It assumes that AZ names won't have uppercase letters, underscores, or other special characters.
	// However, please note that this regex is intentionally lenient and might allow some invalid AZ names through.
	//
	// Keep in mind that while this regex is more general, it might also result in false positives by accepting invalid AZ names.
	// It's important to combine this heuristic with additional checks based on cloud provider documentation or APIs
	// to ensure that only valid AZ names are used in your application.
	azRegexPattern = `^[a-z0-9\-]+$`
	maxAzNameSize  = 128
)

var (
	UnknownAvailabilityZone = "WARPSTREAM_UNSET_AZ"

	gcpMetadataAddress   = "http://169.254.169.254/computeMetadata/v1"
	azureMetadataAddress = "http://169.254.169.254"

	azRegex *regexp.Regexp
)

func init() {
	var err error
	azRegex, err = regexp.Compile(azRegexPattern)
	if err != nil {
		panic(fmt.Errorf("error compiling regex: %w", err))
	}
}

// AvailabilityZone returns the availability zone in which the application is currently
// running.
func AvailabilityZone(ctx context.Context, logger *slog.Logger) (string, error) {
	return availabilityZone(ctx, logger, []azLoaderMethod{
		// Start with k8s because it's the most common for production and cloud-agnostic.
		{name: "k8sZoneName", impl: availabilityZoneNameK8sAPI},
		{name: "awsEC2ZoneName", impl: availabilityZoneNameAWSEC2},
		{name: "awsECSZoneName", impl: availabilityZoneAWSECS},
		{name: "gcp", impl: availabilityZoneGCP},
		{name: "azure", impl: availabilityZoneAzure},
	})
}

// AvailabilityZoneID returns the availability zone ID in which the application is currently
// running. It only differs from AvailabilityZone in AWS, in GCP and Azure it runs the exact
// same code. Note that this method will look for the AZ ID first, but will fallback to the
// AZ Name in AWS if it has to.
func AvailabilityZoneID(ctx context.Context, logger *slog.Logger) (string, error) {
	return availabilityZone(ctx, logger, []azLoaderMethod{
		// Start with zone ID implementations.
		{name: "k8sZoneName", impl: availabilityZoneIDK8sAPI},
		{name: "awsEC2ZoneName", impl: availabilityZoneIDAWSEC2},

		// Then fallback to zone-name implementations.
		{name: "k8sZoneName", impl: availabilityZoneNameK8sAPI},
		{name: "awsEC2ZoneName", impl: availabilityZoneNameAWSEC2},
		{name: "awsECSZoneName", impl: availabilityZoneAWSECS},

		// No concept of zone IDs in GCP/Azure, just use the regular
		// code.
		{name: "gcp", impl: availabilityZoneGCP},
		{name: "azure", impl: availabilityZoneAzure},
	})
}

func availabilityZone(
	ctx context.Context,
	logger *slog.Logger,
	// All the available methods (with a description string that we'll use to prefix the error in case all
	// attempts failed). Use a slice instead of a map so we can control the ordering.
	methods []azLoaderMethod,
) (string, error) {
	// Always honor the environment variable override regardless of which methods are passed.
	az, ok := envvarAvailabilityZone(ctx, logger)
	if ok {
		return az, nil
	}

	var azErrs = make([]error, 0)
	for _, method := range methods {
		if ctx.Err() != nil {
			azErrs = append(azErrs, fmt.Errorf("%s Err: %w", method.name, ctx.Err()))
			continue
		}

		// dedicated deadline for each method
		ctx, cc := context.WithTimeout(ctx, 5*time.Second)
		az, err := method.impl(ctx, logger)
		cc()
		if err == nil {
			return az, nil
		}
		azErrs = append(azErrs, fmt.Errorf("%sErr: %w", method.name, err))
	}

	if len(azErrs) == 0 {
		return UnknownAvailabilityZone, fmt.Errorf("failed to find an az but no error was provided")
	}

	err := errors.Join(azErrs...)
	return UnknownAvailabilityZone, err
}

func envvarAvailabilityZone(ctx context.Context, logger *slog.Logger) (string, bool) {
	if az := warpStreamAvailabilityZone(); az != "" {
		logger.InfoContext(
			ctx,
			"detected availability zone from environment variables",
			slog.String("availability_zone", az))
		return az, true
	}

	return "", false
}

func availabilityZoneNameAWSEC2(
	ctx context.Context,
	logger *slog.Logger,
) (string, error) {
	return availabilityZoneAWSEC2(ctx, logger, false)
}

func availabilityZoneIDAWSEC2(
	ctx context.Context,
	logger *slog.Logger,
) (string, error) {
	return availabilityZoneAWSEC2(ctx, logger, true)
}

func availabilityZoneAWSEC2(
	ctx context.Context,
	_ *slog.Logger,
	useID bool,
) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("error loading default AWS config: %w", err)
	}

	var (
		client = imds.NewFromConfig(cfg)
		input  = &imds.GetMetadataInput{
			Path: "placement/availability-zone",
		}
	)
	if useID {
		input.Path = "placement/availability-zone-id"
	}
	output, err := client.GetMetadata(ctx, input)
	if err != nil {
		return "", fmt.Errorf("error getting metadata: %w", err)
	}

	azB, err := io.ReadAll(output.Content)
	if err != nil {
		return "", fmt.Errorf("error reading availability zone contents: %w", err)
	}

	azs := string(azB)
	if azs == "" {
		return "", fmt.Errorf("got empty availability zone from EC2 metadata")
	}

	if err := ValidateAZ(azs); err != nil {
		return "", fmt.Errorf("error validating availability zone from EC2 metadata: %s: %w", azs, err)
	}

	return azs, nil
}

func availabilityZoneGCP(ctx context.Context, _ *slog.Logger) (string, error) {
	ctx, cc := context.WithTimeout(ctx, 1*time.Second)
	defer cc()

	url := fmt.Sprintf("%s/instance/zone", gcpMetadataAddress)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("error creating request for self availability zone in GCP: %w", err)
	}
	req.Header.Add("Metadata-Flavor", "Google")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error fetching self availability zone in GCP: %w", err)
	}
	defer resp.Body.Close()
	selfAZB, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading self availability zone in GCP from response body: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("error getting availablity zone: %s", string(selfAZB))
	}

	selfAZS := string(selfAZB)
	if selfAZS == "" {
		return "", fmt.Errorf("got empty availability zone from GCP metadata")
	}
	selfAZS = maybeProcessGCPAZ(selfAZS)

	if err := ValidateAZ(selfAZS); err != nil {
		return "", fmt.Errorf("error validating availability zone from GCP metadata: %s: %w", selfAZS, err)
	}

	return selfAZS, nil
}

func availabilityZoneAzure(ctx context.Context, _ *slog.Logger) (string, error) {
	location, err := locationAzure(ctx)
	if err != nil {
		return "", err
	}

	zone, err := zoneAzure(ctx)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s-%s", location, zone), nil
}

func locationAzure(ctx context.Context) (string, error) {
	ctx, cc := context.WithTimeout(ctx, 1*time.Second)
	defer cc()

	baseURL := azureMetadataAddress

	if ctxURL := ctx.Value("url"); ctxURL != nil {
		baseURL = ctxURL.(string)
	}

	locationURL := fmt.Sprintf("%s/metadata/instance/compute/location", baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, locationURL, nil)
	if err != nil {
		return "", fmt.Errorf("error creating request for self location in Azure: %w", err)
	}
	req.Header.Add("Metadata", "true")

	q := req.URL.Query()
	q.Add("format", "text")
	q.Add("api-version", "2024-07-17")
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error fetching self location in Azure: %w", err)
	}
	defer resp.Body.Close()
	selfMetadata, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading self location in Azure from response body: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("error getting location: %s", string(selfMetadata))
	}

	return string(selfMetadata), nil
}

func zoneAzure(ctx context.Context) (string, error) {
	ctx, cc := context.WithTimeout(ctx, 1*time.Second)
	defer cc()

	baseURL := azureMetadataAddress

	if ctxURL := ctx.Value("url"); ctxURL != nil {
		baseURL = ctxURL.(string)
	}

	zoneURL := fmt.Sprintf("%s/metadata/instance/compute/zone", baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zoneURL, nil)
	if err != nil {
		return "", fmt.Errorf("error creating request for self zone in Azure: %w", err)
	}
	req.Header.Add("Metadata", "true")

	q := req.URL.Query()
	q.Add("format", "text")
	q.Add("api-version", "2024-07-17")
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error fetching self zone in Azure: %w", err)
	}
	defer resp.Body.Close()
	selfMetadata, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading self zone in Azure from response body: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("error getting zone: %s", string(selfMetadata))
	}

	return string(selfMetadata), nil
}

// https://github.com/brunoscheufler/aws-ecs-metadata-go/blob/67e37ae746cd/v4.go
const (
	ecsMetadataUriEnvV4 = "ECS_CONTAINER_METADATA_URI_V4"
)

type taskMetadata struct {
	AvailabilityZone string `json:"AvailabilityZone"`
}

func availabilityZoneAWSECS(ctx context.Context, _ *slog.Logger) (string, error) {
	metadataUrl := os.Getenv(ecsMetadataUriEnvV4)
	if metadataUrl == "" {
		return "", fmt.Errorf("missing metadata uri in environment (%s), likely not running in ECS", ecsMetadataUriEnvV4)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		fmt.Sprintf("%s/task", metadataUrl), nil)
	if err != nil {
		return "", fmt.Errorf("error creating request for self availability zone for AWS ECS: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error fetching self availability zone in AWS ECS: %w", err)
	}
	defer resp.Body.Close()
	metadataBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading self availability zone in AWS ECS from response body: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("error getting availablity zone: %s", string(metadataBytes))
	}

	taskMetadata := &taskMetadata{}
	err = json.Unmarshal(metadataBytes, &taskMetadata)
	if err != nil {
		return "", fmt.Errorf("could not unmarshal into task metadata: %w", err)
	}

	if taskMetadata.AvailabilityZone == "" {
		return "", fmt.Errorf("got an empty availability zone from AWS ECS metadata")
	}

	if err := ValidateAZ(taskMetadata.AvailabilityZone); err != nil {
		return "", fmt.Errorf("error validating availability zone from AWS ECS metadata: %s: %w", taskMetadata.AvailabilityZone, err)
	}

	return taskMetadata.AvailabilityZone, nil
}

func availabilityZoneNameK8sAPI(
	ctx context.Context,
	logger *slog.Logger,
) (string, error) {
	return availabilityZoneK8sAPI(ctx, logger, false)
}

func availabilityZoneIDK8sAPI(
	ctx context.Context,
	logger *slog.Logger,
) (string, error) {
	return availabilityZoneK8sAPI(ctx, logger, true)
}

func availabilityZoneK8sAPI(
	ctx context.Context,
	logger *slog.Logger,
	useAZID bool,
) (string, error) {
	var (
		podName = os.Getenv("POD_NAME")
		podNs   = os.Getenv("POD_NAMESPACE")
	)

	if podName == "" || podNs == "" {
		return "", fmt.Errorf("likely not running in a k8s cluster (missing POD_NAME or POD_NAMESPACE environment variable")
	}

	conf, err := restclient.InClusterConfig()
	if err != nil {
		return "", fmt.Errorf("failed to build k8s config: %w", err)
	}

	client, err := kubernetes.NewForConfig(conf)
	if err != nil {
		return "", fmt.Errorf("failed to build k8s client: %w", err)
	}

	var (
		lastErr error
		retries = 0
		// The client may fail to connect to the API server in the first request.
		defaultRetry = wait.Backoff{
			Steps:    10,
			Duration: 100 * time.Millisecond,
			Factor:   1.5,
			Jitter:   0.1,
		}
		v *version.Info
	)

	err = wait.ExponentialBackoff(defaultRetry, func() (bool, error) {
		v, err = client.Discovery().ServerVersion()

		if err == nil {
			return true, nil
		}

		lastErr = err
		retries++
		return false, nil
	})

	// err is returned in case of timeout in the exponential backoff (ErrWaitTimeout)
	if err != nil {
		return "", fmt.Errorf("failed to query k8s api: %w", lastErr)
	}

	logger.InfoContext(ctx, "running in kubernetes cluster", slog.String("version", fmt.Sprintf("%s.%s", v.Major, v.Minor)))

	pod, err := client.CoreV1().Pods(podNs).Get(ctx, podName, v1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("unable to get pod information: %w", err)
	}

	node, err := client.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, v1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("unable to get node information: %w", err)
	}

	label := apiv1.LabelTopologyZone
	if useAZID {
		label = "topology.k8s.aws/zone-id"
	}

	if foundAz, ok := node.GetLabels()[label]; ok {
		logger.InfoContext(ctx, fmt.Sprintf("found this zone on the node labels: %s", foundAz))
		return foundAz, nil
	}

	return "", fmt.Errorf("did not find label on node")
}

// ValidateAZ checks if the provided string matches the regex pattern for a valid availability zone.
func ValidateAZ(input string) error {
	if len(input) > maxAzNameSize {
		return fmt.Errorf("AZ name length must be < %d, but is %d", maxAzNameSize, len(input))
	}
	if !azRegex.MatchString(strings.ToLower(input)) {
		return fmt.Errorf("invalid AZ name not matching standard cloud naming: %s", input)
	}
	return nil
}

func maybeProcessGCPAZ(az string) string {
	// GCP will return something like projects/XXXXXXXXX/zones/us-central1-a
	if strings.HasPrefix(az, "projects") {
		split := strings.Split(az, "/")
		return split[len(split)-1]
	}

	return az
}

// warpStreamAvailabilityZone returns the value of the WARPSTREAM_AVAILABILITY_ZONE.
func warpStreamAvailabilityZone() string {
	return os.Getenv("WARPSTREAM_AVAILABILITY_ZONE")
}

type azLoaderMethod struct {
	name string
	impl func(ctx context.Context, logger *slog.Logger) (string, error)
}
