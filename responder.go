package main

import (
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
	"sync"

	jose "github.com/go-jose/go-jose/v3"
)

type leResponder struct {
	accountKeyThumbprint string // raw base64url encoded thumbprint

	sync.Mutex
	bodies map[string]responseInfo
}

type responseInfo struct {
	body   []byte
	domain string
}

func newLEResponser(accountPubKey *rsa.PublicKey) (*leResponder, error) {
	k := jose.JSONWebKey{Key: accountPubKey}
	thumbprint, err := k.Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, err
	}
	thumbprintB64 := base64.RawURLEncoding.EncodeToString(thumbprint)
	lr := &leResponder{
		accountKeyThumbprint: thumbprintB64,
		bodies:               make(map[string]responseInfo),
	}
	return lr, nil
}

const acmePath = "/.well-known/acme-challenge/"

func (lr *leResponder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Google Load Balancers (well, at least from the kubernetes Ingress
	// API) don't have configurable healthchecks. They also expect / to
	// always return a 200. So, we special case their user agent.
	if r.URL.Path == "/" && r.Header.Get("User-Agent") == "GoogleHC/1.0" {
		w.Write([]byte("OK"))
		return
	}

	if !strings.HasPrefix(r.URL.Path, acmePath) {
		log.Printf("responder received incorrectly prefixed path %s", r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	token := r.URL.Path[len(acmePath):len(r.URL.Path)]
	ok := false
	lr.Lock()
	info, ok := lr.bodies[token]
	lr.Unlock()
	if !ok {
		log.Printf("responder received unknown token path %s", r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	log.Printf("responder received known path (for domain %s) %s", info.domain, r.URL.Path)
	w.Write(info.body)
}

func (lr *leResponder) AddAuthorization(domain, token string) {
	ka := token + "." + lr.accountKeyThumbprint
	lr.Lock()
	defer lr.Unlock()
	lr.bodies[token] = responseInfo{body: []byte(ka), domain: domain}
}

func (lr *leResponder) Reset() {
	lr.Lock()
	defer lr.Unlock()
	lr.bodies = make(map[string]responseInfo)
}
