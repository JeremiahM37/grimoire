package oauth

// ProtectedResourceMetadata is served at
// /.well-known/oauth-protected-resource, per RFC 9728. It is the document a
// client fetches after grimoire-mcp answers a bare request with 401 — see
// the resource_metadata parameter on the WWW-Authenticate header, set in
// internal/mcp/http.go.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
	ResourceName           string   `json:"resource_name,omitempty"`
}

// AuthorizationServerMetadata is served at
// /.well-known/oauth-authorization-server, per RFC 8414.
//
// AuthorizationEndpoint deliberately names a different host than everything
// else in this document: it is the one endpoint a human's browser loads, so
// it points at the private, tailnet-only consent listener
// (GRIMOIRE_OAUTH_AUTHORIZE_BASE), while registration, token issuance and
// revocation — which only the client's backend ever calls — stay on the
// public base this document itself is served from. Nothing in RFC 8414
// requires same-origin endpoints; MCP clients follow the URLs given, not the
// issuer's host.
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	RevocationEndpoint                string   `json:"revocation_endpoint,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}
