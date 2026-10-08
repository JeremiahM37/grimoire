package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tailnet TLS: GRIMOIRE_TLS_PORT makes Grimoire serve HTTPS itself, on the
// node's tailnet addresses only, with a real certificate from tailscaled.
//
// Why not `tailscale serve`: a reverse proxy makes every request arrive from
// loopback, which erases the peer address that GRIMOIRE_IDENTITY=tailscale
// resolves into a name. Terminating TLS here keeps the real tailnet peer as
// RemoteAddr, so attribution is unchanged. Binding only the tailnet IPs (never
// 0.0.0.0) keeps the origin tailnet-only; nothing here touches Funnel.

// localAPI is the slice of tailscaled's LocalAPI this needs.
type localAPI struct {
	base string // "http://local-tailscaled.sock" over a unix dialer, or a test URL
	hc   *http.Client
}

func newLocalAPI(endpoint string) *localAPI {
	if path, ok := strings.CutPrefix(endpoint, "unix://"); ok {
		d := &net.Dialer{}
		return &localAPI{base: "http://local-tailscaled.sock", hc: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "unix", path)
			}},
		}}
	}
	return &localAPI{base: strings.TrimRight(endpoint, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
}

func (l *localAPI) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, l.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Sec-Tailscale", "localapi")
	resp, err := l.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tailscaled %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// self returns this node's MagicDNS name and tailnet addresses.
func (l *localAPI) self() (dns string, ips []string, err error) {
	b, err := l.get("/localapi/v0/status?peers=false")
	if err != nil {
		return "", nil, err
	}
	var st struct {
		Self struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return "", nil, err
	}
	dns = strings.TrimSuffix(st.Self.DNSName, ".")
	if dns == "" || len(st.Self.TailscaleIPs) == 0 {
		return "", nil, errors.New("tailscaled reports no DNS name or tailnet address yet")
	}
	return dns, st.Self.TailscaleIPs, nil
}

// certSource hands out the node certificate, renewing it through tailscaled.
type certSource struct {
	api *localAPI
	dns string

	mu   sync.Mutex
	cert *tls.Certificate
	at   time.Time
}

func (c *certSource) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && time.Since(c.at) < 12*time.Hour {
		return c.cert, nil
	}
	b, err := c.api.get("/localapi/v0/cert/" + c.dns + "?type=pair")
	if err != nil {
		if c.cert != nil { // keep serving the old one rather than fail the handshake
			log.Printf("tailnet TLS: renewal failed, reusing certificate: %v", err)
			return c.cert, nil
		}
		return nil, err
	}
	// type=pair is the private key PEM and the certificate chain PEM in one
	// buffer; X509KeyPair finds each block by type.
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("tailnet TLS: parsing certificate pair: %w", err)
	}
	c.cert, c.at = &cert, time.Now()
	return c.cert, nil
}

// serveTailnetTLS binds each tailnet address on port and serves h over HTTPS.
// It returns the servers so the caller can shut them down.
func serveTailnetTLS(endpoint, port string, h http.Handler) ([]*http.Server, error) {
	api := newLocalAPI(endpoint)
	dns, ips, err := api.self()
	if err != nil {
		return nil, err
	}
	cs := &certSource{api: api, dns: dns}
	if _, err := cs.get(nil); err != nil { // fail loudly at start, not at first visitor
		return nil, err
	}
	var out []*http.Server
	for _, ip := range ips {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, port))
		if err != nil {
			for _, s := range out {
				_ = s.Close()
			}
			return nil, err
		}
		srv := &http.Server{
			Handler:           h,
			TLSConfig:         &tls.Config{GetCertificate: cs.get, MinVersion: tls.VersionTLS12},
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       5 * time.Minute,
			WriteTimeout:      5 * time.Minute,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    1 << 20,
		}
		out = append(out, srv)
		go func() {
			if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("tailnet TLS listener %s: %v", ln.Addr(), err)
			}
		}()
	}
	log.Printf("tailnet TLS: https://%s:%s on %s", dns, port, strings.Join(ips, ", "))
	return out, nil
}
