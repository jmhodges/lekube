package main

import (
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
	"sync"

	jose "gopkg.in/square/go-jose.v1"
)

type leResponder struct {
	accountKeyThumbprint string // raw base64url encoded thumbprint

	sync.Mutex
	bodies map[string][]byte
}

func newLEResponser(accountPubKey *rsa.PublicKey) (*leResponder, error) {
	k := jose.JsonWebKey{Key: accountPubKey}
	thumbprint, err := k.Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, err
	}
	thumbprintB64 := base64.RawURLEncoding.EncodeToString(thumbprint)
	lr := &leResponder{
		accountKeyThumbprint: thumbprintB64,
		bodies:               make(map[string][]byte),
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

	// FIXME add verbose
	log.Printf("responder received %s", r.URL.Path)

	if !strings.HasPrefix(r.URL.Path, acmePath) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	token := r.URL.Path[len(acmePath):len(r.URL.Path)]
	ok := false
	lr.Lock()
	body, ok := lr.bodies[token]
	lr.Unlock()
	if !ok {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Write(body)
}

func (lr *leResponder) AddAuthorization(token string) {
	ka := token + "." + lr.accountKeyThumbprint
	lr.Lock()
	defer lr.Unlock()
	lr.bodies[token] = []byte(ka)
}

func (lr *leResponder) Reset() {
	lr.Lock()
	defer lr.Unlock()
	lr.bodies = make(map[string][]byte)
}
