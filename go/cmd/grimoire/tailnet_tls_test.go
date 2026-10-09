package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakePair(t *testing.T, host string) []byte {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host},
		DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
}

func TestLocalAPISelfAndCert(t *testing.T) {
	pair := fakePair(t, "box.example.ts.net")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Tailscale") != "localapi" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/localapi/v0/status"):
			w.Write([]byte(`{"Self":{"DNSName":"box.example.ts.net.","TailscaleIPs":["100.64.0.1"]}}`))
		case r.URL.Path == "/localapi/v0/cert/box.example.ts.net" && r.URL.Query().Get("type") == "pair":
			w.Write(pair)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	api := newLocalAPI(ts.URL)
	dns, ips, err := api.self()
	if err != nil || dns != "box.example.ts.net" || len(ips) != 1 {
		t.Fatalf("self = %q %v %v", dns, ips, err)
	}
	cs := &certSource{api: api, dns: dns}
	c, err := cs.get(nil)
	if err != nil || c == nil {
		t.Fatalf("cert: %v", err)
	}
	if c2, _ := cs.get(nil); c2 != c {
		t.Fatal("certificate should be cached")
	}
}

func TestSelfRejectsMissingAddress(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Self":{"DNSName":"","TailscaleIPs":[]}}`))
	}))
	defer ts.Close()
	if _, _, err := newLocalAPI(ts.URL).self(); err == nil {
		t.Fatal("expected error before tailscale has an address")
	}
}
