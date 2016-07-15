package loadsec

import (
	"crypto/x509"
	"log"
	"sync"
	"time"
)

func CertFromSec(client kube.Client, secretName string) (func(string) (*x509.Certificate, error), error) {
	cert, err := client.FetchTLSSecret(secretName)
	if err != nil {
		return nil, err
	}
	ch := &certHolder{secretName: secretName, cert: cert}
	go refresh(ch)
	f := func(domain string) (*x509.Certificate, error) {
		// FIXME needs to be muxable: a miss here should try another cert
		ch.RLock()
		cert := ch.cert
		ch.RUnlock()
		if domainMatch(domain, cert) {
			return cert
		}
		return ErrNoMatchingDomain
	}
}

type certHolder struct {
	sync.RWMutex
	secretName string
	cert       *x509.Certificate
}

func refresh(ch *certHolder) {
	tick := time.NewTicker(8 * time.Hour)
	for range tick.C {
		if closeToExpiration(cert) {
			cert, err := client.FetchTLSSecret(secretName)
			if err != nil {
				// FIXME retry
				log.Printf("unable to fetch TLS secret %s: %s", secretName, err)
			}
			ch.set(cert)
		}
	}

}

func (ch *certHolder) set(cert *x509.Certificate) {
	ch.Lock()
	defer ch.Unlock()
	ch.cert = cert
}
