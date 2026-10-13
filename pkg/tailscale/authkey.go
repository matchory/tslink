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
		return "", errors.New("invalid OAuth client secret attribute baseURL")
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
	ephemeralSet             bool // the secret has an ephemeral attribute
	baseURL                  string
}

// The attributes an OAuth client secret may carry.
const (
	attrEphemeral     = "ephemeral"
	attrPreauthorized = "preauthorized"
	attrBaseURL       = "baseURL"
)

// KeyEphemeral reports whether a node registered with key is ephemeral, as
// far as the key itself says, and whether it says so in an ephemeral
// attribute. OAuth client secrets register ephemeral nodes unless
// "?ephemeral=false" is appended; an auth key's ephemerality is set when it is
// created and cannot be read from it, so it counts as not ephemeral. tailscale
// up reads attributes only from OAuth client secrets and passes any other key
// on unchanged, so an attribute appended to an auth key means nothing. The
// error is that of an OAuth client secret with invalid attributes.
func KeyEphemeral(key string) (bool, bool, error) {
	if !strings.HasPrefix(key, "tskey-client-") {
		return false, false, nil
	}
	_, attrs, err := parseClientSecret(key)
	if err != nil {
		return false, false, err
	}
	return attrs.ephemeral, attrs.ephemeralSet, nil
}

// parseClientSecret splits an OAuth client secret from its attributes. Its
// errors quote nothing of s: they end up in the log.
func parseClientSecret(s string) (string, clientSecretAttrs, error) {
	secret, query, _ := strings.Cut(s, "?")
	attrs := clientSecretAttrs{baseURL: "https://api.tailscale.com"}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", attrs, errors.New("invalid OAuth client secret attributes")
	}
	for name := range values {
		switch name {
		case attrEphemeral, attrPreauthorized, attrBaseURL:
		default:
			return "", attrs, errors.New("unknown OAuth client secret attribute")
		}
	}
	attrs.ephemeralSet = values.Has(attrEphemeral)
	if attrs.ephemeral, err = boolAttr(values, attrEphemeral, true); err != nil {
		return "", attrs, err
	}
	if attrs.preauthorized, err = boolAttr(values, attrPreauthorized, false); err != nil {
		return "", attrs, err
	}
	if v := values.Get(attrBaseURL); v != "" {
		attrs.baseURL = v
	}
	return secret, attrs, nil
}

// boolAttr returns the boolean attribute name, or def without one. An empty
// value is invalid, not the default: appending the attribute again, as
// Network.Credential does, would not override it.
func boolAttr(values url.Values, name string, def bool) (bool, error) {
	if !values.Has(name) {
		return def, nil
	}
	b, err := strconv.ParseBool(values.Get(name))
	if err != nil {
		return false, fmt.Errorf("invalid OAuth client secret attribute %s", name)
	}
	return b, nil
}
