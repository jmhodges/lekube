package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	startRenewDur      = flag.Duration("startRenewDur", 3*7*24*time.Hour, "duration before cert expiration to start attempting to renew it")
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
		if secDom.Namespace == nil {
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
	tlsSecs := make(map[string]*tlsSecret)
	okaySecs := []*secretConf{}
	for _, secDom := range conf.Secrets {
		tlsSec, err := fetchTLSSecret(client.Secrets(*secDom.Namespace), secDom.SecretName)
		if err != nil {
			// FIXME mention tls.crt and tls.key in #config-format
			recordError(fetchSecret, "unable to fetch TLS secret value %#v: %s", secDom.SecretName, err)
			continue
		}
		tlsSecs[secDom.SecretName] = tlsSec
		okaySecs = append(okaySecs, secDom)
	}
	for _, secDom := range okaySecs {
		tlsSec := tlsSecs[secDom.SecretName]
		if tlsSec == nil {
			// FIXME remove all of these
			log.Println("nil tlsSec :(")
			continue
		} else {
			log.Println("welp", closeToExpiration(tlsSec.Cert), domainMismatch(tlsSec.Cert, secDom.Domains))
		}

		if tlsSec == nil || closeToExpiration(tlsSec.Cert) || domainMismatch(tlsSec.Cert, secDom.Domains) {
			// newCertPair, err := fetchLECert(acmeClient, email, secDom.Domains)
			// if err != nil {
			// 	recordError(fetchCert, "unable to get Let's Encrypt certificate for %s: %s", secDom.SecretName, err)
			// 	continue
			// }
			// FIXME include kube resource version in cert object so that we
			// can fail on attempting to update a secret that's already been
			// refreshed.
			newCert := tlsSec.Secret.Data["tls.crt"]
			newKey := tlsSec.Secret.Data["tls.key"]
			err := storeTLSSecret(client.Secrets(*secDom.Namespace), tlsSec.Secret, newCert, newKey)
			if err != nil {
				// FIXME handle some other process updating it instead?
				recordError(storeSecret, "unable to store the TLS cert and key as secret %#v: %s", secDom.SecretName, err)
			}
		}
	}
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
	// This confirms the key and cert parse correctly and are both of the right
	// types (RSA, ECDSA). Unfortunately, since the Leaf cert isn't kept in the
	// tls.Certificate (see the docs for tls.X509KeyPair), we have to do that
	// work again to set it.
	tcert, err := tls.X509KeyPair(b, kb)
	if err != nil {
		return nil, fmt.Errorf("unable to parse key pair already in secret %#v, discontinuing to prevent damage: %s", secretName, err)
	}
	block, _ := pem.Decode(b)
	certs, err := x509.ParseCertificates(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unable to parse certificates already in secret %#v, discontinuing to prevent damage: %s", secretName, err)
	}
	tcert.Leaf = certs[0]
	ts := &tlsSecret{
		Cert:   tcert,
		Secret: sec,
	}
	return ts, nil
}

func storeTLSSecret(cl core13.SecretInterface, sec *kubeapi.Secret, newCert []byte, newKey []byte) error {
	sec.Data["tls.crt"] = newCert
	sec.Data["tls.key"] = newKey
	_, err := cl.Update(sec)
	log.Println("Updated", sec.Name)
	return err
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
	Cert tls.Certificate
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

func closeToExpiration(cert tls.Certificate) bool {
	t := time.Now().Add(*startRenewDur)
	log.Println(t, cert.Leaf.NotAfter)
	return t.Equal(cert.Leaf.NotAfter) || t.After(cert.Leaf.NotAfter)
}

func domainMismatch(cert tls.Certificate, domains []string) bool {
	cdoms := []string{}
	cdoms = append(cdoms, cert.Leaf.Subject.CommonName)
	cdoms = append(cdoms, cert.Leaf.DNSNames...)
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
	Namespace  *string  `json:"namespace"`
	SecretName string   `json:"secret_name"` // name of the secret
	Domains    []string `json:"domains"`     // FIXME check for empty strings
}
