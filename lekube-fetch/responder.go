package main

import (
	"crypto"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/context"

	jose "gopkg.in/square/go-jose.v1"
)

type tokData struct {
	body     []byte
	notifier chan<- bool
}

type leResponder struct {
	accountKeyThumbprint string // raw base64url encoded thumbprint

	sync.Mutex
	tokens map[string]tokData
}

func newLEResponser(accountKey crypto.PrivateKey) (*leResponder, error) {
	k := jose.JsonWebKey{Key: accountKey}
	thumbprint, err := k.Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, err
	}
	thumbprintB64 := base64.RawURLEncoding.EncodeToString(thumbprint)
	lr := &leResponder{
		accountKeyThumbprint: thumbprintB64,
		tokens:               make(map[string]tokData),
	}
	return lr, nil
}

const acmePath = "/.well-known/acme-challenge/"

func (lr *leResponder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// FIXME remove
	log.Printf("responder received %s", r.URL.Path)
	if !strings.HasPrefix(r.URL.Path, acmePath) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	token := r.URL.Path[0:len(acmePath)]
	ok := false
	lr.Lock()
	tokData, ok := lr.tokens[token]
	lr.Unlock()
	if !ok {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Write(tokData.body)
	select {
	case tokData.notifier <- true:
	default:
		// In case we've not pulled the last notification, just fall
		// through. This assumes notifier has a buffer of 1.
	}
}

func (lr *leResponder) Authorize(ctx context.Context, token string) error {
	ka := token + "." + lr.accountKeyThumbprint
	info := map[string]interface{}{
		"resource":         "challenge",
		"type":             "http",
		"keyAuthorization": ka,
	}

	bb, err := json.Marshal(&info)
	if err != nil {
		return err
	}

	notifier := make(chan bool, 1)
	lr.Lock()
	lr.tokens[token] = tokData{
		body:     bb,
		notifier: notifier,
	}
	lr.Unlock()
	select {
	case <-notifier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (lr *leResponder) Reset() {
	lr.Lock()
	defer lr.Unlock()
	lr.tokens = make(map[string]tokData)
}
