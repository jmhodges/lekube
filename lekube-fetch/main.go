package main

import (
	"crypto/x509"
	"encoding/json"
	"flag"
	"log"
	"os"
	"strings"
	"time"
)

var (
	legoDir  = flag.String("legoAccountDir", "", "directory to find Let's Encrypt account files stored by lego")
	confPath = flag.String("conf", "", "path to required JSON config file described by https://github.com/jmhodges/lesec/#config-format")
	server   = flag.String("server", "acme-v01.api.letsencrypt.org", "ACME API server domain to use for fetching certificates")
)

func main() {
	flag.Parse()
	if *confPath == "" {
		log.Printf("-conf flag is required")
		flag.Usage()
		os.Exit(2)
	}
	secs := make(map[string]bool)
	conf, err := unmarshalConf(*confPath)
	if err != nil {
		log.Fatalf("unable to parse config file %#v: %s", *confPath, err)
	}
	for i, secDom := range conf.Secrets {
		if secDom.SecretName == "" {
			log.Fatalf("no name given for secret config at index %d in \"secrets\"", i)
		}
		if secs[secDom.SecretName] {
			log.Fatalf("duplicate config for secret %s", secDom.SecretName)
		}
		secs[secDom.SecretName] = true
		if len(secDom.Domains) == 0 {
			log.Fatalf("no domains given for secret %s", secDom.SecretName)
		}
		for j, d := range secDom.Domains {
			d = strings.TrimSpace(d)
			if d == "" {
				log.Fatal("empty string in domains of secret config at index %d in \"secrets\"", j)
			}
			secDom.Domains[j] = d
		}
	}
	tick := time.NewTicker(8 * time.Hour)
	run(conf)
	for range tick.C {
		run(conf)
	}

}
func run(conf *allConf) {
	certs := make(map[string]*x509.Certificate)
	okaySecs := []secretConf{}
	for _, secDom := range conf.Secrets {
		cert, err := fetchTLSSecret(secDom.SecretName)
		if err != nil {
			// FIXME mention tls.crt and tls.key in #config-format
			recordError(fetchSecret, "unable to fetch TLS secret value", err)
			continue
		}
		certs[secDom.SecretName] = cert
		okaySecs := append(okaySecs, secDom)
	}
	for _, secDom := range okaySecs {
		cert := certs[secDom]
		if cert == nil || closeToExpiration(cert) || domainMismatch(cert, secs[secDom.Domains]) {
			newCertPair, err := fetchLECert(secDom.Domains)
			if err != nil {
				recordError(fetchCert, "unable to get Let's Encrypt certificate for %s: %s", secDom.SecretName, err)
				continue
			}
			// FIXME include kube resource version in cert object so that we
			// can fail on attempting to update a secret that's already been
			// refreshed.
			err = storeTLSSecret(secDom.SecretName, cert, newCertPair)
			if err != nil {
				// FIXME handle some other process updating it instead?
				recordError(storeSecret, "unable to store the TLS cert and key as secret %s: %s", err)
			}
		}
	}
}

func fetchTLSSecret(secretName string) (*tlsSecret, error) {

}

func unmarshalConf(fp string) (*allConf, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	conf := &allConf{}
	err = json.NewDecoder(f).Decode(conf)
	return ac, err
}

type allConf struct {
	Secrets []secretConf `json:"secrets"`
}

type secretConf struct {
	// FIXME Namespace string
	SecretName string   // name of the secret
	Domains    []string // FIXME check for empty strings
}
