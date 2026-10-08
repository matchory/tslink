package tailscale

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	tsapi "tailscale.com/client/tailscale/v2"
)

// resolveAuthKey turns an OAuth client secret into a single-use auth key for
// tags, as "tailscale up" does and the Kubernetes operator does for its
// proxies. Other keys are returned unchanged.
//
// The secret may carry attributes, as for "tailscale up": ephemeral (default
// true), preauthorized (default false) and baseURL, the API server.
func resolveAuthKey(ctx context.Context, authKey string, tags []string) (string, error) {
	if !strings.HasPrefix(authKey, "tskey-client-") {
		return authKey, nil
	}
	if len(tags) == 0 {
		return "", errors.New("an OAuth client secret needs tags")
	}
	secret, attrs, err := parseClientSecret(authKey)
	if err != nil {
		return "", err
	}
	baseURL, err := url.Parse(attrs.baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid baseURL: %w", err)
	}
	client := &tsapi.Client{
		BaseURL:   baseURL,
		UserAgent: "tslink",
		// The client ID is ignored: the secret names the client
		Auth: &tsapi.OAuth{ClientID: "tslink", ClientSecret: secret},
	}
	var caps tsapi.KeyCapabilities
	caps.Devices.Create.Ephemeral = attrs.ephemeral
	caps.Devices.Create.Preauthorized = attrs.preauthorized
	caps.Devices.Create.Tags = tags
	key, err := client.Keys().CreateAuthKey(ctx, tsapi.CreateKeyRequest{Capabilities: caps})
	if err != nil {
		return "", fmt.Errorf("failed to create an auth key with the OAuth client: %w", err)
	}
	return key.Key, nil
}

// clientSecretAttrs are the attributes an OAuth client secret may carry.
type clientSecretAttrs struct {
	ephemeral, preauthorized bool
	baseURL                  string
}

// parseClientSecret splits an OAuth client secret from its attributes.
func parseClientSecret(s string) (string, clientSecretAttrs, error) {
	secret, query, _ := strings.Cut(s, "?")
	attrs := clientSecretAttrs{ephemeral: true, baseURL: "https://api.tailscale.com"}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", attrs, fmt.Errorf("invalid OAuth client secret attributes: %w", err)
	}
	for name := range values {
		v := values.Get(name)
		switch name {
		case "ephemeral", "preauthorized":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return "", attrs, fmt.Errorf("invalid OAuth client secret attribute %s=%q", name, v)
			}
			if name == "ephemeral" {
				attrs.ephemeral = b
			} else {
				attrs.preauthorized = b
			}
		case "baseURL":
			attrs.baseURL = v
		default:
			return "", attrs, fmt.Errorf("unknown OAuth client secret attribute %q", name)
		}
	}
	return secret, attrs, nil
}
