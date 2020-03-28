// blah test travis
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"golang.org/x/crypto/acme"
	"golang.org/x/time/rate"
)

type leClient struct {
	cl              *limitedACMEClient
	dir             acme.Directory
	registrationURI string
	responder       *leResponder
}

func (lc *leClient) CreateCert(ctx context.Context, sconf *secretConf) (*newCert, error) {
	if len(sconf.Domains) == 0 {
		return nil, fmt.Errorf("cannot request a certificate with no names")
	}
	domains := uniqueDomains(sconf.Domains)

	type domErr struct {
		dom     string
		err     error
		authURI string
	}
	log.Printf("attempting to authorize secret %s with domains %s", sconf.FullName(), domains)
	order, err := lc.authorizeDomains(ctx, domains)
	if err != nil {
		err = fmt.Errorf("in secret %s, failed to authorize order of domains %s: %s", sconf.FullName(), domains, err)

		return nil, err
	}

	var priv crypto.PrivateKey
	var pblock *pem.Block
	var sigAlg x509.SignatureAlgorithm
	if sconf.UseRSA {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		pblock = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
		priv = k
		sigAlg = x509.SHA256WithRSA
	} else {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		b, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, err
		}
		pblock = &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
		priv = k
		sigAlg = x509.ECDSAWithSHA256
	}
	keyOut := &bytes.Buffer{}
	err = pem.Encode(keyOut, pblock)
	if err != nil {
		return nil, err
	}

	csrDER, err := createCSR(sconf.Domains, priv, sigAlg)
	if err != nil {
		return nil, err
	}

	certDERs, _, err := lc.cl.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return nil, err
	}
	pemCerts := [][]byte{}
	for _, c := range certDERs {
		block := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: c,
		}
		pemCerts = append(pemCerts, pem.EncodeToMemory(block))
	}
	nc := &newCert{
		Cert: bytes.Join(pemCerts, []byte{'\n'}),
		Key:  keyOut.Bytes(),
	}
	return nc, nil
}

func (lc *leClient) authorizeDomains(ctx context.Context, domains []string) (*acme.Order, error) {
	authzIDs := make([]acme.AuthzID, len(domains))
	for i, dom := range domains {
		authzIDs[i] = acme.AuthzID{Type: "dns", Value: dom}
	}
	order, err := lc.cl.AuthorizeOrder(ctx, authzIDs)
	if err != nil {
		log.Printf("error during AuthorizeOrder call for domains %s: %s (%#v)", domains, err, err)
		return nil, err
	}

	for i, azURL := range order.AuthzURLs {
		a, err := lc.cl.GetAuthorization(ctx, azURL)
		if err != nil {
			log.Printf("error during GetAuthorization call for authz url %s (likely for domain %s): %s", azURL, domains[i], err)
		}
		ch, err := findChallenge(a)
		if err != nil {
			return nil, fmt.Errorf("unable to find matching challenge for authz of domain %s (authz URL %s): %s", a.Identifier.Value, azURL, err)
		}
		log.Printf("adding authorization for %#v, token %#v, authz url %s", a.Identifier.Value, ch.Token, a.URI)
		lc.responder.AddAuthorization(a.Identifier.Value, ch.Token)
		_, err = lc.cl.Accept(ctx, ch)
		if err != nil {
			return nil, fmt.Errorf("error during Accept of challenge for %s: %s", a.Identifier.Value, err)
		}
	}

	afterOrder, err := lc.cl.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("error during WaitOrder for domains %s, order URI %s: %s", domains, order.URI, err)
	}
	if afterOrder.Status == acme.StatusInvalid {
		return nil, fmt.Errorf("authorization marked as invalid")
	}
	if afterOrder.Status != acme.StatusReady {
		return nil, fmt.Errorf("authorization order URI %s: want state %s, got %s at timeout expiration", order.URI, acme.StatusReady, afterOrder.Status)
	}
	return afterOrder, nil
}

func createCSR(domains []string, priv crypto.PrivateKey, sigAlg x509.SignatureAlgorithm) ([]byte, error) {
	csr := &x509.CertificateRequest{
		SignatureAlgorithm: sigAlg,

		Subject:  pkix.Name{CommonName: domains[0]},
		DNSNames: domains,
	}

	return x509.CreateCertificateRequest(rand.Reader, csr, priv)
}

func findChallenge(a *acme.Authorization) (*acme.Challenge, error) {
	seen := make([]string, 0, len(a.Challenges))
	for _, ch := range a.Challenges {
		if ch.Type == "http-01" {
			return ch, nil
		}
		seen = append(seen, ch.Type)
	}
	return nil, fmt.Errorf("no http-01 challenges in %#v", seen)
}

// leClientMaker allows us to change the ACME (Let's Encrypt) API url and
// account email without restarting lekube by creating a new account if need
// be. It ensures that a) the acme.Client's private key has been registered with
// the given ACME API and b) the account has a current Terms of Service enabled.
type leClientMaker struct {
	httpClient *http.Client
	accountKey *rsa.PrivateKey
	responder  *leResponder
	// limit is to match to the request-per-IP (supposedly,
	// request-per-IP-per-endpoint, but it didn't seem to be) nginx rate limit
	// Let's Encrypt put in place across all accounts and clients.
	limit *rate.Limiter

	infoToClient map[accountInfo]*leClient
}

func newLEClientMaker(c *http.Client, accountKey *rsa.PrivateKey, responder *leResponder, limiter *rate.Limiter) *leClientMaker {
	return &leClientMaker{
		httpClient:   c,
		accountKey:   accountKey,
		responder:    responder,
		limit:        limiter,
		infoToClient: make(map[accountInfo]*leClient),
	}
}

type accountInfo struct {
	directoryURL string
	email        string
}

type clientAndRegURI struct {
	leClient        *leClient
	registrationURI string
}

func (lcm *leClientMaker) Make(ctx context.Context, directoryURL, email string) (*leClient, error) {
	if len(directoryURL) == 0 {
		return nil, errors.New("directoryURL of Let's Encrypt API may not be blank")
	}

	// Trim trailing slashes off to prevent folks sliding it in and out of their
	// configs and creating duplicate accounts that we don't need.
	directoryURL = strings.TrimRight(directoryURL, "/")
	info := accountInfo{directoryURL, email}
	lc, ok := lcm.infoToClient[info]
	if ok {
		return lc, ensureTermsOfUse(ctx, lc)
	}

	cl := &limitedACMEClient{
		limit: lcm.limit,
		cl: &acme.Client{
			Key:          lcm.accountKey,
			HTTPClient:   lcm.httpClient,
			DirectoryURL: directoryURL,
		},
	}
	dir, err := cl.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to discover ACME endpoints at directory URL %s: %s", directoryURL, err)
	}

	acc := &acme.Account{
		Contact: []string{"mailto:" + email},
	}
	acc, err = cl.Register(ctx, acc, acme.AcceptTOS)
	if err != nil {
		return nil, fmt.Errorf("unable to create new registration: %s", err)
	}
	leClient := &leClient{
		cl:              cl,
		dir:             dir,
		responder:       lcm.responder,
		registrationURI: acc.URI,
	}
	lcm.infoToClient[info] = leClient
	return leClient, nil
}

func ensureTermsOfUse(ctx context.Context, lc *leClient) error {
	acc, err := lc.cl.GetReg(ctx, lc.registrationURI)
	if err != nil {
		return fmt.Errorf("unable to refresh account info while determining most recent Terms of Service: %s", err)
	}

	if acc.CurrentTerms != acc.AgreedTerms {
		acc.AgreedTerms = acc.CurrentTerms
		_, err := lc.cl.UpdateReg(ctx, acc)
		if err != nil {
			return fmt.Errorf("unable to update registration for new agreement terms: %s", err)
		}
	}
	return nil
}

// uniqueDomains removes duplicate domains by removing any duplicates after the
// first, avoiding accidental order changes that might affect the CN.
func uniqueDomains(doms []string) []string {
	ds := make(map[string]bool)
	newDoms := []string{}
	for _, d := range doms {
		if !ds[d] {
			newDoms = append(newDoms, d)
			ds[d] = true
		}
	}
	return newDoms
}

type limitedACMEClient struct {
	limit *rate.Limiter
	cl    *acme.Client
}

func (lac *limitedACMEClient) Discover(ctx context.Context) (acme.Directory, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return acme.Directory{}, err
	}
	return lac.cl.Discover(ctx)
}

func (lac *limitedACMEClient) CreateOrderCert(ctx context.Context, url string, csr []byte, bundle bool) (der [][]byte, certURL string, err error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, "", err
	}
	return lac.cl.CreateOrderCert(ctx, url, csr, bundle)
}

func (lac *limitedACMEClient) AuthorizeOrder(ctx context.Context, id []acme.AuthzID, opt ...acme.OrderOption) (*acme.Order, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}

	return lac.cl.AuthorizeOrder(ctx, id, opt...)
}

func (lac *limitedACMEClient) Accept(ctx context.Context, chal *acme.Challenge) (*acme.Challenge, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}
	return lac.cl.Accept(ctx, chal)
}

func (lac *limitedACMEClient) GetAuthorization(ctx context.Context, url string) (*acme.Authorization, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}
	return lac.cl.GetAuthorization(ctx, url)
}

func (lac *limitedACMEClient) GetReg(ctx context.Context, url string) (*acme.Account, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}
	return lac.cl.GetReg(ctx, url)
}

func (lac *limitedACMEClient) UpdateReg(ctx context.Context, a *acme.Account) (*acme.Account, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}
	return lac.cl.UpdateReg(ctx, a)
}

func (lac *limitedACMEClient) Register(ctx context.Context, a *acme.Account, prompt func(tosURL string) bool) (*acme.Account, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}
	return lac.cl.Register(ctx, a, prompt)
}

func (lac *limitedACMEClient) WaitOrder(ctx context.Context, url string) (*acme.Order, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}
	return lac.cl.WaitOrder(ctx, url)
}
