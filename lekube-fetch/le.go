package main

import (
	"bytes"
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

	"github.com/cenk/backoff"
	"github.com/google/acme"
)

type leClient struct {
	cl        *acme.Client
	ep        *acme.Endpoint
	responder *leResponder
}

func (lc *leClient) createCert(sconf *secretConf, alreadyAuthDomains map[string]bool) (*newCert, error) {
	type domErr struct {
		dom string
		err error
	}
	authResps := []chan domErr{}
	for _, dom := range sconf.Domains {
		if alreadyAuthDomains[dom] {
			continue
		}
		log.Printf("attempting to authorize %s:%s", sconf.FullName(), dom)
		ch := make(chan domErr, 1)
		authResps = append(authResps, ch)
		go func(dom string) {
			a, err := lc.authorizeDomain(dom)
			if err != nil {
				log.Printf("failed to authorize domain %s:%s: %s", sconf.FullName(), dom, a.URI)
			} else {
				log.Printf("authorized domain %s:%s: %s", sconf.FullName(), dom, a.URI)
			}
			ch <- domErr{dom, err}
		}(dom)
	}

	for _, ch := range authResps {
		de := <-ch
		if de.err != nil {
			return nil, de.err
		}
		alreadyAuthDomains[de.dom] = true
	}

	if len(sconf.Domains) == 0 {
		return nil, fmt.Errorf("cannot request a certificate with no names")
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

	certDERs, _, err := lc.cl.CreateCert(lc.ep.CertURL, csrDER, 0, true)
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

func (lc *leClient) authorizeDomain(dom string) (*acme.Authorization, error) {
	a, err := lc.cl.Authorize(lc.ep.AuthzURL, dom)
	if err != nil {
		return nil, err
	}
	ch, err := findChallenge(a)
	if err != nil {
		return nil, err
	}
	log.Printf("adding authorization for %#v: token %#v", dom, ch.Token)
	lc.responder.AddAuthorization(ch.Token)
	_, err = lc.cl.Accept(ch)
	if err != nil {
		return nil, fmt.Errorf("error during Accept of challenge: %s", err)
	}
	var a2 *acme.Authorization
	b := backoff.NewExponentialBackOff()
	op := func() error {
		var err error
		a2, err = lc.cl.GetAuthz(a.URI)
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
			return &a.Challenges[comb[0]], nil
		}
	}
	return nil, fmt.Errorf("no challenge combination of just http. challenges: %s, combinations: %v", a.Challenges, a.Combinations)
}
