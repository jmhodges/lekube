package main

import (
	"crypto"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

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

func newLEResponser(accountPubKey interface{}) (*leResponder, error) {
	k := jose.JsonWebKey{Key: accountPubKey}
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
	token := r.URL.Path[len(acmePath):len(r.URL.Path)]
	ok := false
	lr.Lock()
	tokData, ok := lr.tokens[token]
	lr.Unlock()
	if !ok {
		log.Printf("didn't find %#v in map", token)
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

func (lr *leResponder) AddAuthorization(token string) {
	ka := token + "." + lr.accountKeyThumbprint
	info := map[string]interface{}{
		"resource":         "challenge",
		"type":             "http-01",
		"keyAuthorization": ka,
	}

	bb, err := json.Marshal(&info)
	if err != nil {
		// This shouldn't happen unless we really screw up the code, so a panic
		// is fine.
		panic("unable to marshal key authorization: " + err.Error())
	}

	notifier := make(chan bool, 1)
	lr.Lock()
	log.Printf("adding for real %#v, and body %#v", token, string(bb))
	lr.tokens[token] = tokData{
		body:     bb,
		notifier: notifier,
	}
	lr.Unlock()
}

func (lr *leResponder) Reset() {
	lr.Lock()
	defer lr.Unlock()
	lr.tokens = make(map[string]tokData)
}
