package main

import (
	"crypto/x509"
	"flag"
	"log"
	"os"
)

var (
	legoDir  = flag.String("legoAccountDir", "", "directory to find Let's Encrypt account files stored by lego")
	confPath = flag.String("conf", "", "path to required JSON config file described by https://github.com/jmhodges/lesec/#config-format")
)

func main() {
	flag.Parse()
	if *confPath == "" {
		log.Printf("-conf flag is required")
		flag.Usage()
		os.Exit(2)
	}
	secs := make(map[string]bool)
	for i, secDom := range conf.Secrets {
		if secDom.Secret == "" {
			log.Fatalf("no name given for secret config at index %d in \"secrets\"", i)
		}
		if secs[secDom.Secret] {
			log.Fatalf("duplicate config for secret %s", secDom.Secret)
		}
		secs[secDom.Secret] = true
		if len(secDom.Domains) == "" {
			log.Fatalf("no domains given for secret %s", secDom.Secret)
		}
	}
	for {
		certs := make(map[string]*x509.Certificate)
		okaySecs := []secretConf{}
		for _, secDom := range conf.Secrets {
			cert, err := fetchTLSSecret(secDom.Secret)
			if err != nil {
				// FIXME mention tls.crt and tls.key in #config-format
				recordError(fetchSecret, "unable to fetch TLS secret value", err)
				continue
			}
			certs[secDom.Secret] = cert
			okaySecs := append(okaySecs, secDom)
		}
		for _, secDom := range okaySecs {
			cert := certs[secDom]
			if cert == nil || closeToExpiration(cert) || domainMismatch(cert, secs[secDom.Domains]) {
				newCertPair, err := fetchLECert(secDom.Domains)
				if err != nil {
					recordError(fetchCert, "unable to get Let's Encrypt certificate for %s: %s", secDom.Secret, err)
					continue
				}
				err = storeTLSSecret(secDom.Secret, newCertPair)
				if err != nil {
					// FIXME handle some other process updating it instead?
					recordError(storeSecret, "unable to store the TLS cert and key as secret %s: %s", err)
				}
			}
		}
	}
}
