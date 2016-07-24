package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/acme"

	kerrors "k8s.io/kubernetes/pkg/api/errors"
	unversioned "k8s.io/kubernetes/pkg/api/unversioned"
	kubeapi "k8s.io/kubernetes/pkg/api/v1"
	kube13 "k8s.io/kubernetes/pkg/client/clientset_generated/release_1_3"
	core13 "k8s.io/kubernetes/pkg/client/clientset_generated/release_1_3/typed/core/v1"
	"k8s.io/kubernetes/pkg/client/restclient"
)

var (
	confPath         = flag.String("conf", "", "path to required JSON config file described by https://github.com/jmhodges/lekube/#config-format")
	useProd          = flag.Bool("prod", false, "if given, use the production Let's Encrypt API instead of the default staging API")
	startRenewDur    = flag.Duration("startRenewDur", 3*7*24*time.Hour, "duration before cert expiration to start attempting to renew it")
	betweenChecksDur = flag.Duration("betweenChecksDur", 8*time.Hour, "duration to wait before checking to see if any of the TLS secrets have expired")
	httpAddr         = flag.String("addr", ":10080", "address to boot the HTTP server on")

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

	type nsSecName struct {
		ns   string
		name string
	}
	secs := make(map[nsSecName]bool)
	conf, err := unmarshalConf(*confPath)
	if err != nil {
		log.Fatalf("unable to parse config file %#v: %s", *confPath, err)
	}
	if conf.Email == "" {
		log.Fatalf("'email' must be set in the config file %#v", *confPath)
	}

	for i, secConf := range conf.Secrets {
		if secConf.SecretName == "" {
			log.Fatalf("no SecretName given for secret config at index %d in \"secrets\"", i)
		}
		if secConf.Namespace == nil {
			log.Fatalf("no Namespace given for secret config at index %d in \"secrets\"", i)
		}
		name := nsSecName{*secConf.Namespace, secConf.SecretName}
		if secs[name] {
			log.Fatalf("duplicate config for secret %s", secConf.SecretName)
		}
		secs[name] = true
		if len(secConf.Domains) == 0 {
			log.Fatalf("no domains given for secret %s", secConf.SecretName)
		}
		for j, d := range secConf.Domains {
			d = strings.TrimSpace(d)
			if d == "" {
				log.Fatal("empty string in domains of secret config at index %d in \"secrets\"", j)
			}
			secConf.Domains[j] = d
		}
	}
	accountKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("unable to generate private account key (not a TLS private key) for the Let's Encrypt account: %s", err)
	}
	httpClient := &http.Client{
		Timeout: 20 * time.Second,
	}

	acmeClient := &acme.Client{
		Key:    accountKey,
		Client: *httpClient,
	}
	responder, err := newLEResponser(&accountKey.PublicKey)
	if err != nil {
		log.Fatalf("unable to make responder: %s", err)
	}

	restConfig, err := restclient.InClusterConfig()
	if err != nil {
		log.Fatalf("unable to make config for kubernetes client: %s", err)
	}

	client := kube13.NewForConfigOrDie(restConfig).Core()
	tick := time.NewTicker(*betweenChecksDur)

	directoryURL := "https://acme-staging.api.letsencrypt.org/directory"
	if *useProd {
		directoryURL = "https://acme-v01.api.letsencrypt.org/directory"
	}
	ep, err := acme.Discover(httpClient, directoryURL)
	if err != nil {
		log.Fatalf("unable to discover ACME endpoints: %s", err)
	}
	// FIXME check for an account being saved already
	// FIXME support old regs
	acc := &acme.Account{
		Contact:     []string{fmt.Sprintf("mailto:%s", conf.Email)},
		AgreedTerms: "https://letsencrypt.org/documents/LE-SA-v1.0.1-July-27-2015.pdf",
	}

	err = acmeClient.Register(ep.RegURL, acc)
	if err != nil {
		log.Fatalf("unable to create new registration: %s", err)
	}
	if acc.CurrentTerms != acc.AgreedTerms {
		acc.AgreedTerms = acc.CurrentTerms
		// FIXME update reg shouldn't be able to take ep.RegURL
		err = acmeClient.UpdateReg(acc.URI, acc)
		log.Printf("current agreement url: %s", acc.CurrentTerms)
		if err != nil {
			log.Fatalf("unable to update registration for new agreement terms: %s", err)
		}
	}
	ch := make(chan error)
	go func() {
		ch <- http.ListenAndServe(*httpAddr, responder)
	}()

	go func() {
		run(acmeClient, &ep, responder, client, conf)
		for range tick.C {
			run(acmeClient, &ep, responder, client, conf)
		}
	}()
	err = <-ch
	if err != nil {
		log.Fatal(err)
	}
}

func run(acmeClient *acme.Client, ep *acme.Endpoint, responder *leResponder, client core13.CoreInterface, conf *allConf) {
	responder.Reset()
	tlsSecs := make(map[string]*tlsSecret)
	okaySecs := []*secretConf{}
	alreadyAuthDomains := make(map[string]bool)

	for _, secConf := range conf.Secrets {
		tlsSec, err := fetchTLSSecret(client.Secrets(*secConf.Namespace), secConf.SecretName)
		if err != nil {
			// FIXME mention tls.crt and tls.key in #config-format
			recordError(fetchSecret, "unable to fetch TLS secret value %#v: %s", secConf.SecretName, err)
			continue
		}
		tlsSecs[secConf.SecretName] = tlsSec
		okaySecs = append(okaySecs, secConf)
	}
	for _, secConf := range okaySecs {
		tlsSec := tlsSecs[secConf.SecretName]

		if tlsSec == nil || closeToExpiration(tlsSec.Cert) || domainMismatch(tlsSec.Cert, secConf.Domains) {
			leCert, err := fetchLECert(acmeClient, ep, responder, secConf, alreadyAuthDomains)
			if err != nil {
				recordError(fetchCert, "unable to get Let's Encrypt certificate for %s: %s", secConf.SecretName, err)
				continue
			}
			var oldSec *kubeapi.Secret
			if tlsSec != nil {
				oldSec = tlsSec.Secret
			}
			err = storeTLSSecret(client.Secrets(*secConf.Namespace), secConf, oldSec, leCert)
			if err != nil {
				// FIXME handle some other process updating it instead?
				recordError(storeSecret, "unable to store the TLS cert and key as secret %#v: %s", secConf.SecretName, err)
			}
		}
	}
}

// fetchTLSSecret may return a nil tlsSecret if no secret was found.
func fetchTLSSecret(client core13.SecretInterface, secretName string) (*tlsSecret, error) {
	sec, err := client.Get(secretName)
	if err != nil {
		serr, ok := err.(*kerrors.StatusError)
		if ok && serr.ErrStatus.Reason == unversioned.StatusReasonNotFound {
			return nil, nil
		}
		return nil, err
	}
	// If there's no cert data already in the Secret, we'll assume the user knew
	// what they were doing and multiple bits of private data inside the same
	// Secret and so return nil.
	b, ok := sec.Data["tls.crt"]
	if !ok {
		return nil, nil
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

func storeTLSSecret(cl core13.SecretInterface, secConf *secretConf, oldSec *kubeapi.Secret, leCert *newCert) error {
	f := cl.Update
	sec := oldSec
	if oldSec == nil {
		f = cl.Create
		sec = &kubeapi.Secret{
			ObjectMeta: kubeapi.ObjectMeta{
				Name: secConf.SecretName,
			},
			Data: make(map[string][]byte),
		}
	}

	sec.Data["tls.crt"] = leCert.Cert
	sec.Data["tls.key"] = leCert.Key
	_, err := f(sec)
	return err
}

func fetchLECert(cl *acme.Client, ep *acme.Endpoint, responder *leResponder, sconf *secretConf, alreadyAuthDomains map[string]bool) (*newCert, error) {
	for _, dom := range sconf.Domains {
		if alreadyAuthDomains[dom] {
			continue
		}
		log.Printf("attempting to authorize %#v", dom)
		a, err := cl.Authorize(ep.AuthzURL, dom)
		if err != nil {
			return nil, err
		}
		ch, err := findChallenge(a)
		if err != nil {
			return nil, err
		}
		log.Printf("adding authorization for %#v: token %#v", dom, ch.Token)
		responder.AddAuthorization(ch.Token)
		_, err = cl.Accept(ch)
		if err != nil {
			return nil, fmt.Errorf("error during Accept of challenge: %s", err)
		}
		var a2 *acme.Authorization
		endTime := time.Now().Add(10 * time.Minute) // FIXME config?
		for time.Now().Before(endTime) {
			log.Printf("Looking up auth for %#v: %s", dom, a.URI)
			a2, err = cl.GetAuthz(a.URI)
			if a2.Status == acme.StatusValid {
				log.Printf("Valid auth for %#v found", dom)
				break
			}
			if a2.Status == acme.StatusInvalid {
				log.Printf("authorization went invalid for %s", dom)
				break
			}
			// FIXME exponential backoff
			time.Sleep(5 * time.Second)
		}
		if err != nil {
			return nil, err
		}
		if a2 == nil {
			return nil, errors.New("a nil authorization happened somehow")
		}
		if a2.Status != acme.StatusValid {
			return nil, fmt.Errorf("authorization for %#v in state %s at timeout expiration", dom, a2.Status)
		}
		log.Printf("WORKED %s: %s", dom, a2.URI)
		alreadyAuthDomains[dom] = true
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

	certDERs, _, err := cl.CreateCert(ep.CertURL, csrDER, 0, true)
	if err != nil {
		return nil, err
	}
	cert := []byte{}
	for _, c := range certDERs {
		block := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: c,
		}
		cert = append(cert, '\n')
		cert = append(cert, pem.EncodeToMemory(block)...)
	}
	nc := &newCert{
		Cert: cert,
		Key:  keyOut.Bytes(),
	}
	return nc, nil
}

type newCert struct {
	Cert []byte // PEM encoded bytes of the TLS cert and the cert chain needed to resolve it correctly.
	Key  []byte // PEM encoded bytes of the TLS private key generated
}

type tlsSecret struct {
	Cert tls.Certificate
	*kubeapi.Secret
}

func createCSR(domains []string, priv crypto.PrivateKey, sigAlg x509.SignatureAlgorithm) ([]byte, error) {
	sans := []string{}
	if len(domains) > 1 {
		sans = domains[1:len(domains)]
	}
	csr := &x509.CertificateRequest{
		SignatureAlgorithm: sigAlg,

		Subject:  pkix.Name{CommonName: domains[0]},
		DNSNames: sans,
	}

	return x509.CreateCertificateRequest(rand.Reader, csr, priv)
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
	Email   string        `json:"email"`
	Secrets []*secretConf `json:"secrets"`
}

type secretConf struct {
	Namespace  *string  `json:"namespace"`
	SecretName string   `json:"secret_name"` // FIXME change to name / name of the secret
	Domains    []string `json:"domains"`     // FIXME check for empty strings
	UseRSA     bool     // use ECDSA in the certs if false, RSA for certs
}

func findChallenge(a *acme.Authorization) (*acme.Challenge, error) {
	for _, comb := range a.Combinations {
		if len(comb) == 1 && a.Challenges[comb[0]].Type == "http-01" {
			return &a.Challenges[comb[0]], nil
		}
	}
	return nil, fmt.Errorf("no challenge combination of just http. challenges: %s, combinations: %v", a.Challenges, a.Combinations)
}
