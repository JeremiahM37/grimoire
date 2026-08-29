package identity

import (
	"crypto/x509"
	"net/http"
	"net/netip"
	"strings"
)

// MTLS identifies a caller by the client certificate the TLS handshake already
// verified.
//
// The strongest backend here and the only one that needs no overlay network,
// no daemon and no external service: if the connection completed with a client
// certificate, Go's TLS stack has already checked it against the configured
// ClientCAs, and the name in it is as good as the CA that signed it. It is
// also the only one that works from anywhere, which matters for an agent
// running somewhere the operator does not control the network.
//
// The cost is that somebody has to run a CA. That is why this is one backend
// among several rather than the answer.
type MTLS struct {
	// Field selects what names the caller: "cn" (the subject common name),
	// "dns" (the first DNS SAN), "email", or "uri". Defaults to cn.
	Field string
}

func (m *MTLS) Name() string { return "mtls" }

func (m *MTLS) Identify(_ netip.Addr, r *http.Request) (Identity, bool) {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Identity{}, false
	}
	// VerifiedChains is empty when the server accepted a certificate without
	// verifying it (ClientAuth below RequireAndVerifyClientCert). An
	// unverified certificate is a string the caller chose, so it must not
	// produce a verified identity.
	if len(r.TLS.VerifiedChains) == 0 {
		return Identity{}, false
	}
	cert := r.TLS.PeerCertificates[0]
	name := certName(cert, m.Field)
	if name == "" {
		return Identity{}, false
	}
	device := name
	if cn := strings.TrimSpace(cert.Subject.CommonName); cn != "" {
		device = cn
	}
	return Identity{
		Subject:  "mtls:" + name,
		Device:   device,
		User:     firstOr(cert.EmailAddresses, ""),
		Backend:  "mtls",
		Verified: true,
	}, true
}

func certName(c *x509.Certificate, field string) string {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "dns":
		return firstOr(c.DNSNames, "")
	case "email":
		return firstOr(c.EmailAddresses, "")
	case "uri":
		if len(c.URIs) > 0 {
			return c.URIs[0].String()
		}
		return ""
	default:
		return strings.TrimSpace(c.Subject.CommonName)
	}
}

func firstOr(ss []string, fallback string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return fallback
}
