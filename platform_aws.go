package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// platformAWSMode selects SigV4-signed forwarding to Claude Platform on AWS.
// platformAWSModeOff and the empty string both disable it (first-party
// passthrough) regardless of the region/workspace vars. A rollback must clear
// ANTHROPIC_AWS_ROLE_ARN before blanking or disabling ANTHROPIC_AWS_MODE.
const (
	platformAWSMode    = "platform"
	platformAWSModeOff = "off"
)

// platformAWSService is the SigV4 service name for Claude Platform on AWS.
const platformAWSService = "aws-external-anthropic"

// anthropicVersionDefault is the anthropic-version sent when the client omits it.
const anthropicVersionDefault = "2023-06-01"

// platformAWSState holds the resources for signing Anthropic requests to Claude
// Platform on AWS, initialized at startup. Mirrors bedrockState.
type platformAWSState struct {
	region      string
	workspaceID string
	credProv    aws.CredentialsProvider
	signer      *v4.Signer
}

// initPlatformAWS initializes platform-on-AWS signing resources. The default AWS
// credential chain remains the source identity. When roleARN is set, only cached,
// renewing credentials for that role are used to sign Platform requests.
func initPlatformAWS(region, workspaceID, roleARN string) (*platformAWSState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	credentials := cfg.Credentials
	if roleARN != "" {
		credentials = newPlatformAWSAssumeRoleCredentials(sts.NewFromConfig(cfg), roleARN)
	}

	return &platformAWSState{
		region:      region,
		workspaceID: workspaceID,
		credProv:    credentials,
		signer:      v4.NewSigner(),
	}, nil
}

func newPlatformAWSAssumeRoleCredentials(client stscreds.AssumeRoleAPIClient, roleARN string) aws.CredentialsProvider {
	provider := stscreds.NewAssumeRoleProvider(client, roleARN, func(options *stscreds.AssumeRoleOptions) {
		options.Duration = time.Hour
		options.RoleSessionName = "llm-proxy-platform"
	})
	return aws.NewCredentialsCache(provider)
}

// applyPlatformAWS rewrites an outbound anthropic passthrough request to target
// Claude Platform on AWS and SigV4-signs it. The proxy is the signer, so it
// drops the client's x-api-key/Authorization, points the request at the AWS
// endpoint (preserving the /v1/... path), adds the workspace header, and signs
// over the upstream host. The logging/session variables in ServeHTTP are left
// untouched, so session identity and Loki logging are preserved exactly.
func (p *Proxy) applyPlatformAWS(proxyReq *http.Request, reqBody []byte) error {
	st := p.platformAWS
	host := fmt.Sprintf("%s.%s.api.aws", platformAWSService, st.region)

	proxyReq.URL.Scheme = "https"
	proxyReq.URL.Host = host
	proxyReq.Host = host

	// The proxy authenticates via SigV4; the client's Anthropic credentials must
	// not be forwarded or signed.
	proxyReq.Header.Del("X-Api-Key")
	proxyReq.Header.Del("Authorization")

	proxyReq.Header.Set("Anthropic-Workspace-Id", st.workspaceID)
	if proxyReq.Header.Get("Anthropic-Version") == "" {
		proxyReq.Header.Set("Anthropic-Version", anthropicVersionDefault)
	}

	// Strip hop-by-hop headers BEFORE signing. Go's transport rewrites or drops
	// these in transit (e.g. an SDK-sent Connection header arrives at AWS with an
	// empty value), so signing over them yields a SignedHeaders set that no longer
	// matches what is transmitted → SignatureDoesNotMatch (401).
	stripHopByHopHeaders(proxyReq.Header)

	creds, err := st.credProv.Retrieve(proxyReq.Context())
	if err != nil {
		return fmt.Errorf("retrieve AWS credentials: %w", err)
	}
	bodyHash := sha256Hex(reqBody)
	if err := st.signer.SignHTTP(proxyReq.Context(), creds, proxyReq, bodyHash, platformAWSService, st.region, time.Now()); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}
	return nil
}

// platformAWSErrorSummary preserves actionable failure categories without
// logging provider messages, which may contain credentials or response bodies.
func platformAWSErrorSummary(err error) string {
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		// Error codes also come from the provider, so only known codes are safe
		// to emit. Unknown codes must not become a second error-message channel.
		switch code := apiErr.ErrorCode(); code {
		case "AccessDenied", "AccessDeniedException", "ExpiredToken", "ExpiredTokenException",
			"InvalidClientTokenId", "SignatureDoesNotMatch", "Throttling", "ThrottlingException":
			return "AWS " + code
		default:
			return "AWS API error"
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "network error"
	}
	return "credential or signing error"
}

// sha256Hex returns the hex-encoded SHA256 of the payload, the form SigV4 uses
// for the canonical request's payload hash.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// hopByHopHeaders are the connection-scoped headers that must not be SigV4-signed:
// Go's transport rewrites or drops them in transit, so signing over them makes the
// SignedHeaders set diverge from what the upstream receives.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHopHeaders removes hop-by-hop headers from h, including any header
// named in the Connection header's value (RFC 7230 §6.1). Header.Del canonicalizes
// keys, so the mixed-case names above match regardless of the sender's casing.
func stripHopByHopHeaders(h http.Header) {
	for _, connVal := range h.Values("Connection") {
		for _, name := range strings.Split(connVal, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}
