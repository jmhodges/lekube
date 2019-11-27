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
	"time"

	"github.com/cenkalti/backoff"
	"golang.org/x/crypto/acme"
	"golang.org/x/time/rate"
)

type leClient struct {
	cl              *limitedACMEClient
	dir             acme.Directory
	registrationURI string
	responder       *leResponder
}

func (lc *leClient) CreateCert(ctx context.Context, sconf *secretConf, alreadyAuthDomains map[string]bool) (*newCert, error) {
	if len(sconf.Domains) == 0 {
		return nil, fmt.Errorf("cannot request a certificate with no names")
	}
	domains := uniqueDomains(sconf.Domains)

	type domErr struct {
		dom     string
		err     error
		authURI string
	}
	authResps := []chan domErr{}
	for _, dom := range domains {
		if alreadyAuthDomains[dom] {
			continue
		}
		log.Printf("attempting to authorize %s:%s", sconf.FullName(), dom)
		ch := make(chan domErr, 1)
		authResps = append(authResps, ch)
		go func(dom string) {
			a, err := lc.authorizeDomain(ctx, dom)
			de := domErr{dom: dom, err: err}
			if err == nil {
				de.authURI = a.URI
			}
			ch <- de
		}(dom)
	}

	errs := []string{}
	for _, ch := range authResps {
		var de domErr
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case de = <-ch:
		}

		if de.err == nil {
			log.Printf("authorized domain %s:%s: %s", sconf.FullName(), de.dom, de.authURI)
			alreadyAuthDomains[de.dom] = true
		} else {
			msg := fmt.Sprintf("in secret %s, failed to authorize domain %s: %s", sconf.FullName(), de.dom, de.err)
			errs = append(errs, msg)
		}
	}

	if len(errs) != 0 {
		return nil, fmt.Errorf("%s", errs)
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
	err := pem.Encode(keyOut, pblock)
	if err != nil {
		return nil, err
	}

	csrDER, err := createCSR(sconf.Domains, priv, sigAlg)
	if err != nil {
		return nil, err
	}

	certDERs, _, err := lc.cl.CreateCert(ctx, csrDER, 0, true)
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

func (lc *leClient) authorizeDomain(ctx context.Context, dom string) (*acme.Authorization, error) {
	a, err := lc.cl.Authorize(ctx, dom)
	if err != nil {
		log.Printf("error during actual Authorize call for %#v: %s (%#v)", dom, err, err)
		return nil, err
	}
	ch, err := findChallenge(a)
	if err != nil {
		return nil, err
	}
	log.Printf("adding authorization for %#v, token %#v, url %s", dom, ch.Token, a.URI)
	lc.responder.AddAuthorization(dom, ch.Token)
	_, err = lc.cl.Accept(ctx, ch)
	if err != nil {
		return nil, fmt.Errorf("error during Accept of challenge: %s", err)
	}
	var a2 *acme.Authorization
	b := backoff.WithContext(backoff.NewExponentialBackOff(), ctx)
	op := func() error {
		var err error
		log.Printf("getting authorization for %#v, token %#v, url %s", dom, ch.Token, a.URI)
		a2, err = lc.cl.GetAuthorization(ctx, a.URI)
		if err != nil {
			return err
		}
		if a2.Status == acme.StatusValid || a2.Status == acme.StatusInvalid {
			return nil
		}
		return errors.New("authorization still pending")
	}
	err = backoff.Retry(op, b)
	if err != nil {
		return nil, err
	}
	if a2 == nil {
		return nil, errors.New("a nil authorization happened somehow")
	}
	if a2.Status == acme.StatusInvalid {
		return nil, fmt.Errorf("authorization marked as invalid")
	}
	if a2.Status != acme.StatusValid {
		return nil, fmt.Errorf("authorization for %#v in state %s at timeout expiration", dom, a2.Status)
	}
	return a2, nil
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
	for _, comb := range a.Combinations {
		if len(comb) == 1 && a.Challenges[comb[0]].Type == "http-01" {
			return a.Challenges[comb[0]], nil
		}
	}
	return nil, fmt.Errorf("no challenge combination of just http. challenges: %v, combinations: %v", a.Challenges, a.Combinations)
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

func (lac *limitedACMEClient) CreateCert(ctx context.Context, csr []byte, exp time.Duration, bundle bool) (der [][]byte, certURL string, err error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, "", err
	}
	return lac.cl.CreateCert(ctx, csr, exp, bundle)
}

func (lac *limitedACMEClient) Authorize(ctx context.Context, domain string) (*acme.Authorization, error) {
	if err := lac.limit.Wait(ctx); err != nil {
		return nil, err
	}

	return lac.cl.Authorize(ctx, domain)
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
