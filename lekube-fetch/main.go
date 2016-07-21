package main

import (
	"crypto/x509"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	kubeapi "k8s.io/kubernetes/pkg/api/v1"
	kube13 "k8s.io/kubernetes/pkg/client/clientset_generated/release_1_3"
	core13 "k8s.io/kubernetes/pkg/client/clientset_generated/release_1_3/typed/core/v1"
	"k8s.io/kubernetes/pkg/client/restclient"
)

var (
	confPath           = flag.String("conf", "", "path to required JSON config file described by https://github.com/jmhodges/lekube/#config-format")
	server             = flag.String("server", "acme-v01.api.letsencrypt.org", "ACME API server domain to use for fetching certificates")
	startRenewDur      = flag.Duration("renewCertDur", 3*7*24*time.Hour, "duration before cert expiration to start attempting to renew it")
	betweenChecksDur   = flag.Duration("betweenChecksDur", 8*time.Hour, "duration to wait before checking to see if any of the TLS secrets have expired")
	fetchSecretErrors  = &expvar.Int{}
	fetchCertErrors    = &expvar.Int{}
	storeSecretErrors  = &expvar.Int{}
	fetchSecretMetrics = (&expvar.Map{}).Init()
	fetchCertMetrics   = (&expvar.Map{}).Init()
	storeSecretMetrics = (&expvar.Map{}).Init()
	stageMetrics       = expvar.NewMap("")
)

func main() {
	flag.Parse()
	if *confPath == "" {
		log.Printf("-conf flag is required")
		flag.Usage()
		os.Exit(2)
	}
	fetchSecretMetrics.Set("errors", fetchSecretErrors)
	fetchCertMetrics.Set("errors", fetchCertErrors)
	storeSecretMetrics.Set("errors", storeSecretErrors)
	stageMetrics.Set("fetchSecret", fetchSecretMetrics)
	stageMetrics.Set("fetchCert", fetchCertMetrics)
	stageMetrics.Set("storeSecret", storeSecretMetrics)

	secs := make(map[string]bool)
	conf, err := unmarshalConf(*confPath)
	if err != nil {
		log.Fatalf("unable to parse config file %#v: %s", *confPath, err)
	}
	for i, secDom := range conf.Secrets {
		if secDom.SecretName == "" {
			log.Fatalf("no SecretName given for secret config at index %d in \"secrets\"", i)
		}
		if secDom.Namespace == "" {
			log.Fatalf("no Namespace given for secret config at index %d in \"secrets\"", i)
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
	restConfig, err := restclient.InClusterConfig()
	if err != nil {
		log.Fatalf("unable to make config for kubernetes client: %s", err)
	}
	client := kube13.NewForConfigOrDie(restConfig).Core()
	tick := time.NewTicker(*betweenChecksDur)
	run(client, conf)
	for range tick.C {
		run(client, conf)
	}

}
func run(client core13.CoreInterface, conf *allConf) {
	certs := make(map[string]*tlsSecret)
	okaySecs := []*secretConf{}
	for _, secDom := range conf.Secrets {
		cert, err := fetchTLSSecret(client.Secrets(secDom.Namespace), secDom.SecretName)
		if err != nil {
			// FIXME mention tls.crt and tls.key in #config-format
			recordError(fetchSecret, "unable to fetch TLS secret value %#v: %s", secDom.SecretName, err)
			continue
		}
		certs[secDom.SecretName] = cert
		okaySecs = append(okaySecs, secDom)
	}
	fmt.Println(certs)
	// FIXME uncomment this
	// for _, secDom := range okaySecs {
	// 	cert := certs[secDom.SecretName]
	// 	if cert == nil || closeToExpiration(cert) || domainMismatch(cert, secDom.Domains) {
	// 		newCertPair, err := fetchLECert(acmeClient, email, secDom.Domains)
	// 		if err != nil {
	// 			recordError(fetchCert, "unable to get Let's Encrypt certificate for %s: %s", secDom.SecretName, err)
	// 			continue
	// 		}
	// 		// FIXME include kube resource version in cert object so that we
	// 		// can fail on attempting to update a secret that's already been
	// 		// refreshed.
	// 		err = storeTLSSecret(secDom.SecretName, cert, newCertPair)
	// 		if err != nil {
	// 			// FIXME handle some other process updating it instead?
	// 			recordError(storeSecret, "unable to store the TLS cert and key as secret %#v: %s", secDom.SecretName, err)
	// 		}
	// 	}
	// }
}

func fetchTLSSecret(client core13.SecretInterface, secretName string) (*tlsSecret, error) {
	sec, err := client.Get(secretName)
	if err != nil {
		return nil, err
	}
	kb, ok := sec.Data["tls.key"]
	if !ok {
		return nil, fmt.Errorf("secret %#v has no tls.key", secretName)
	}
	b, ok := sec.Data["tls.crt"]
	if !ok {
		return nil, fmt.Errorf("secret %#v has no tls.crt", secretName)
	}
	cert, err := x509.ParseCertificate(b)
	if err != nil {
		return nil, fmt.Errorf("unable to parse certificate already in secret, discontinuing to prevent damage: %s", err)
	}
	_, err = x509.ParsePKCS1PrivateKey(kb)
	if err != nil {
		return nil, fmt.Errorf("unable to parse private key already in secret, discontinuing to prevent damage: %s", err)
	}

	ts := &tlsSecret{
		Cert:   cert,
		Secret: sec,
	}
	return ts, nil
}

// FIXME also uncomment this
// func fetchLECert(cl *acmeapi.Client, email string, domains []string) (*newTLSSecret, error) {
// 	// FIXME do this just once
// 	e, err := acme.Discover(nil, "https://acme-v01.api.letsencrypt.org")
// 	if err != nil {
// 		return nil, err
// 	}
// 	ac := &acmeapi.Registration{
// 		Resource:    "new-reg", // FIXME support old regs
// 		ContactURIs: []string{fmt.Sprintf("mailto:", email)},
// 		Key:         jwk,
// 	}
// 	err = cl.UpsertRegistration(reg, ctx)
// 	if err != nil {
// 		return nil, err
// 	}
// 	for _, dom := range domains {
// 		ccfg := responder.ChallengeConfig{
// 			HTTPPort: []string{"0.0.0.0:10080"},
// 		}
// 		solver.Authorize(c, dom, ccfg, ctx)
// 	}

// 	if len(domains) == 0 {
// 		return nil, fmt.Errorf("cannot request a certificate with no names")
// 	}
// 	csrDER, err := createCSR()
// 	if err != nil {
// 		return nil, err
// 	}
// 	acrt, err := cl.RequestCertificate(csrDER, ctx)
// 	if err != nil {
// 		return nil, err
// 	}

// 	err = cl.WaitForCertificate(acrt, ctx)
// 	if err != nil {
// 		return nil, err
// 	}

// 	certs := [][]byte{acrt.Certificate}
// 	certs = append(certs, acrt.ExtraCertificates...)
// 	return bytes.Join(certs, []byte{'\n'}), nil
// }

type newTLSSecret struct {
	CertData []byte // The cert and the cert chain needed to resolve it correctly.
	KeyData  []byte // The private key generated
	*kubeapi.Secret
}

type tlsSecret struct {
	Cert *x509.Certificate
	*kubeapi.Secret
}

type stage int

const (
	fetchSecret stage = iota
	fetchCert
	storeSecret
)

var stageErrors = map[stage]*expvar.Int{
	fetchSecret: fetchSecretErrors,
	fetchCert:   fetchCertErrors,
	storeSecret: storeSecretErrors,
}

func recordError(st stage, format string, args ...interface{}) {
	stageErrors[st].Add(1)
	log.Printf(format, args...)
}

func closeToExpiration(sec *tlsSecret) bool {
	t := time.Now().Add(*startRenewDur)
	return t.Equal(sec.Cert.NotAfter) || t.After(sec.Cert.NotAfter)
}

func domainMismatch(sec *tlsSecret, domains []string) bool {
	cdoms := []string{}
	cdoms = append(cdoms, sec.Cert.Subject.CommonName)
	cdoms = append(cdoms, sec.Cert.DNSNames...)
	sort.Strings(cdoms)
	doms := make([]string, len(domains))
	// sort.Strings has side-effects on domains, but fetchLECert uses the order
	// to determine which domain should be the common name in the cert. So, we
	// have to copy domains to doms.
	copy(doms, domains)
	sort.Strings(domains)
	return !reflect.DeepEqual(cdoms, domains)
}

func unmarshalConf(fp string) (*allConf, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	conf := &allConf{}
	err = json.NewDecoder(f).Decode(conf)
	return conf, err
}

type allConf struct {
	Secrets []*secretConf `json:"secrets"`
}

type secretConf struct {
	Namespace  string   `json:"namespace"`
	SecretName string   `json:"secret_name"` // name of the secret
	Domains    []string `json:"domains"`     // FIXME check for empty strings
}
