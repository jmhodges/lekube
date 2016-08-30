package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"expvar"
	"flag"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	kerrors "k8s.io/kubernetes/pkg/api/errors"
	unversioned "k8s.io/kubernetes/pkg/api/unversioned"
	kubeapi "k8s.io/kubernetes/pkg/api/v1"
	kube13 "k8s.io/kubernetes/pkg/client/clientset_generated/release_1_3"
	core13 "k8s.io/kubernetes/pkg/client/clientset_generated/release_1_3/typed/core/v1"
	"k8s.io/kubernetes/pkg/client/restclient"
)

var (
	confPath      = flag.String("conf", "", "path to required JSON config file described by https://github.com/jmhodges/lekube/#config-format")
	startRenewDur = flag.Duration("startRenewDur", 3*7*24*time.Hour, "duration before cert expiration to start attempting to renew it")
	httpAddr      = flag.String("addr", ":10080", "address to boot the HTTP server on")
	httpsAddr     = flag.String("httpsAddr", ":10443", "address to boot the HTTPS server on")
	leTimeoutDur  = flag.Duration("leTimeout", 30*time.Minute, "max time to spend fetching and creating a certificate (but not time spent fetching and storing secrets)")

	fetchSecretAttempts  = &expvar.Int{}
	fetchLECertAttempts  = &expvar.Int{}
	storeSecretAttempts  = &expvar.Int{}
	loadConfigAttempts   = &expvar.Int{}
	fetchSecretErrors    = &expvar.Int{}
	fetchLECertErrors    = &expvar.Int{}
	storeSecretErrors    = &expvar.Int{}
	loadConfigErrors     = &expvar.Int{}
	fetchSecretSuccesses = &expvar.Int{}
	fetchLECertSuccesses = &expvar.Int{}
	storeSecretSuccesses = &expvar.Int{}
	loadConfigSuccesses  = &expvar.Int{}
	runCount             = &expvar.Int{}
	errorCount           = &expvar.Int{}
	fetchSecretMetrics   = (&expvar.Map{}).Init()
	fetchLECertMetrics   = (&expvar.Map{}).Init()
	storeSecretMetrics   = (&expvar.Map{}).Init()
	loadConfigMetrics    = (&expvar.Map{}).Init()
	stageMetrics         = expvar.NewMap("stages")
	buildSHA             = "<debug>"
)

func main() {
	flag.Parse()
	if *confPath == "" {
		log.Printf("-conf flag is required")
		flag.Usage()
		os.Exit(2)
	}

	cLoader, conf, err := newConfLoader(*confPath)
	if err != nil {
		log.Fatalf("unable to load configuration: %s", err)
	}

	fetchSecretMetrics.Set("attempts", fetchSecretAttempts)
	fetchLECertMetrics.Set("attempts", fetchLECertAttempts)
	storeSecretMetrics.Set("attempts", storeSecretAttempts)
	loadConfigMetrics.Set("attempts", cLoader.attempts)

	fetchSecretMetrics.Set("errors", fetchSecretErrors)
	fetchLECertMetrics.Set("errors", fetchLECertErrors)
	storeSecretMetrics.Set("errors", storeSecretErrors)
	loadConfigMetrics.Set("errors", loadConfigErrors)

	fetchSecretMetrics.Set("successes", fetchSecretSuccesses)
	fetchLECertMetrics.Set("successes", fetchLECertSuccesses)
	storeSecretMetrics.Set("successes", storeSecretSuccesses)
	loadConfigMetrics.Set("successes", cLoader.successes)

	stageMetrics.Set("fetch_secret", fetchSecretMetrics)
	stageMetrics.Set("fetch_le_cert", fetchLECertMetrics)
	stageMetrics.Set("store_secret", storeSecretMetrics)
	stageMetrics.Set("load_config", loadConfigMetrics)

	stageMetrics.Set("runs", runCount)
	stageMetrics.Set("errors", errorCount)

	loadConfigMetrics.Set("last_config_check", cLoader.lastCheck)
	loadConfigMetrics.Set("last_config_check_str", unixTime{unixEpoch: cLoader.lastCheck})
	loadConfigMetrics.Set("last_config_change", cLoader.lastChange)
	loadConfigMetrics.Set("last_config_change_str", unixTime{unixEpoch: cLoader.lastChange})

	accountKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("unable to generate private account key (not a TLS private key) for the Let's Encrypt account: %s", err)
	}
	httpClient := &http.Client{
		Timeout: 20 * time.Second,
	}

	responder, err := newLEResponser(&accountKey.PublicKey)
	if err != nil {
		log.Fatalf("unable to make responder: %s", err)
	}

	restConfig, err := restclient.InClusterConfig()
	if err != nil {
		log.Fatalf("unable to make config for kubernetes client: %s", err)
	}

	kubeClient := kube13.NewForConfigOrDie(restConfig).Core()

	lcm := newLEClientMaker(httpClient, accountKey, responder)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = lcm.Make(ctx, dirURLFromConf(conf), conf.Email)
	if err != nil {
		log.Fatalf("unable to make an account with %s using email %s: %s", dirURLFromConf(conf), conf.Email, err)
	}
	cancel()

	m := http.NewServeMux()
	m.HandleFunc("/debug/", func(w http.ResponseWriter, r *http.Request) {
		conf := cLoader.Get()
		if !conf.AllowRemoteDebug && isBlockedRequest(r) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/debug/build" {
			w.Write([]byte("SHA: " + buildSHA))
			return
		}
		http.DefaultServeMux.ServeHTTP(w, r)
	})

	m.Handle("/", responder)

	watchCh := make(chan *allConf)
	runCh := make(chan *allConf)

	go func() {
		for {
			watchCh <- cLoader.Watch()
		}
	}()
	go func() {
		conf := conf
		runCh <- conf
		t := time.NewTicker(1 * time.Hour)
		for {
			select {
			case <-t.C:
			case conf = <-watchCh:
			}
			runCh <- conf
		}
	}()
	go func() {
		for conf := range runCh {
			run(lcm, kubeClient, conf, *leTimeoutDur)
		}
	}()

	if conf.TLSDir != "" {
		go func() {
			crt := filepath.Join(conf.TLSDir, "tls.crt")
			key := filepath.Join(conf.TLSDir, "tls.key")
			err := http.ListenAndServeTLS(*httpsAddr, crt, key, m)
			if err != nil {
				log.Fatalf("unable to boot HTTPS server: %s", err)
			}
		}()
	}

	err = http.ListenAndServe(*httpAddr, m)
	if err != nil {
		log.Fatalf("unable to boot HTTP server: %s", err)
	}
}

func run(lcm *leClientMaker, client core13.CoreInterface, conf *allConf, leTimeout time.Duration) {
	runCount.Add(1)
	lcm.responder.Reset()
	tlsSecs := make(map[nsSecName]*tlsSecret)
	okaySecs := []*secretConf{}
	for _, secConf := range conf.Secrets {
		log.Printf("Fetching kubernetes secret %s", secConf.FullName())
		fetchSecretAttempts.Add(1)
		tlsSec, err := fetchK8SSecret(client.Secrets(*secConf.Namespace), secConf.Name)
		if err != nil {
			recordError(fetchSecStage, "unable to fetch TLS secret value %#v: %s", secConf.Name, err)
			continue
		}
		fetchSecretSuccesses.Add(1)
		log.Printf("Fetched kubernetes secret %s", secConf.FullName())

		tlsSecs[secConf.FullName()] = tlsSec
		okaySecs = append(okaySecs, secConf)
	}

	alreadyAuthDomains := make(map[string]bool)
	for _, secConf := range okaySecs {
		log.Printf("doing work on %s", secConf.FullName())
		tlsSec := tlsSecs[secConf.FullName()]
		if tlsSec == nil || tlsSec.Cert == nil || closeToExpiration(tlsSec.Cert) || domainMismatch(tlsSec.Cert, secConf.Domains) {
			workOn(tlsSec, secConf, alreadyAuthDomains, lcm, client, conf, leTimeout)
		} else {
			log.Printf("no work needed for secret %s", secConf.FullName())
		}

	}
}
func workOn(tlsSec *tlsSecret, secConf *secretConf, alreadyAuthDomains map[string]bool, lcm *leClientMaker, client core13.CoreInterface, conf *allConf, leTimeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), leTimeout)
	defer cancel()

	fetchLECertAttempts.Add(1)
	acmeClient, err := lcm.Make(ctx, dirURLFromConf(conf), conf.Email)
	if err != nil {
		recordError(fetchLECertStage, "unable to get client for Let's Encrypt API that is up to date: %s", err)
		return
	}
	leCert, err := acmeClient.CreateCert(ctx, secConf, alreadyAuthDomains)
	if err != nil {
		recordError(fetchLECertStage, "unable to get Let's Encrypt certificate for %s: %s", secConf.FullName(), err)
		return
	}
	fetchLECertSuccesses.Add(1)
	log.Printf("have new cert for %s", secConf.FullName())
	var oldSec *kubeapi.Secret
	if tlsSec != nil {
		oldSec = tlsSec.Secret
	}

	storeSecretAttempts.Add(1)
	err = storeK8SSecret(client.Secrets(*secConf.Namespace), secConf, oldSec, leCert)
	if err != nil {
		recordError(storeSecStage, "unable to store the TLS cert and key as secret %#v: %s", secConf.Name, err)
		return
	}
	storeSecretSuccesses.Add(1)
	log.Printf("successfully stored new cert in %s", secConf.FullName())
}

// fetchK8SSecret may return a nil tlsSecret if no secret was found.
func fetchK8SSecret(client core13.SecretInterface, secretName string) (*tlsSecret, error) {
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
	block, _ := pem.Decode(b)
	certs, err := x509.ParseCertificates(block.Bytes)
	if err != nil {
		// unable to parse certificates already in the Secret, but we don't
		// actually need it to do our work.
		return &tlsSecret{Secret: sec}, nil
	}

	tlsSec := &tlsSecret{Secret: sec}
	// Find the leaf cert. The order people store the certs is not always the
	// correct order, especially if they were doing things manually for a
	// while. If all of the certs are CA certs, we let ourselves overwrite
	// tls.crt in the secret.
	for _, c := range certs {
		if !c.IsCA {
			tlsSec.Cert = c
			break
		}
	}

	return tlsSec, nil
}

func storeK8SSecret(cl core13.SecretInterface, secConf *secretConf, oldSec *kubeapi.Secret, leCert *newCert) error {
	f := cl.Update
	sec := oldSec
	if oldSec == nil {
		f = cl.Create
		sec = &kubeapi.Secret{
			ObjectMeta: kubeapi.ObjectMeta{
				Name: secConf.Name,
			},
			Data: make(map[string][]byte),
		}
	}

	sec.Data["tls.crt"] = leCert.Cert
	sec.Data["tls.key"] = leCert.Key
	_, err := f(sec)
	return err
}

type newCert struct {
	Cert []byte // PEM encoded bytes of the TLS cert and the cert chain needed to resolve it correctly.
	Key  []byte // PEM encoded bytes of the TLS private key generated
}

type tlsSecret struct {
	Cert *x509.Certificate
	*kubeapi.Secret
}

type stage int

const (
	fetchSecStage stage = iota
	fetchLECertStage
	storeSecStage
	loadConfigStage
)

var stageErrors = map[stage]*expvar.Int{
	fetchSecStage:    fetchSecretErrors,
	fetchLECertStage: fetchLECertErrors,
	storeSecStage:    storeSecretErrors,
	loadConfigStage:  loadConfigErrors,
}

func recordError(st stage, format string, args ...interface{}) {
	errorCount.Add(1)
	stageErrors[st].Add(1)
	log.Printf(format, args...)
}

func closeToExpiration(cert *x509.Certificate) bool {
	t := time.Now().Add(*startRenewDur)
	return t.Equal(cert.NotAfter) || t.After(cert.NotAfter)
}

func domainMismatch(cert *x509.Certificate, domains []string) bool {
	// Since the CommonName can also be in the SAN, let's unique the domains by
	// using maps instead of sorting some slices.
	cdoms := make(map[string]struct{})
	doms := make(map[string]struct{})
	cdoms[cert.Subject.CommonName] = struct{}{}
	for _, d := range cert.DNSNames {
		cdoms[d] = struct{}{}
	}
	for _, d := range domains {
		doms[d] = struct{}{}
	}
	return !reflect.DeepEqual(cdoms, doms)
}

func isBlockedRequest(r *http.Request) bool {
	if r.URL.Path == "/debug" || strings.HasPrefix(r.URL.Path, "/debug/") {
		i := strings.Index(r.RemoteAddr, ":")
		if i < 0 {
			return false
		}
		return !net.ParseIP(r.RemoteAddr[:i]).IsLoopback()
	}
	return false
}

var _ expvar.Var = &unixEpoch{}
var _ expvar.Var = &unixTime{}

// unixEpoch is a nanoseconds since Unix epoch variable that satisfies the expvar.Var interface.
type unixEpoch struct {
	i int64
}

func (v *unixEpoch) String() string {
	return strconv.FormatInt(atomic.LoadInt64(&v.i), 10)
}

func (v *unixEpoch) Set(value int64) {
	atomic.StoreInt64(&v.i, value)
}

// unixTime is a wrapper for unixEpoch to format its string as a
// RFC3339 timestamp that satisfies the expvar.Var interface
type unixTime struct {
	*unixEpoch
}

func (u unixTime) String() string {
	return time.Unix(0, u.unixEpoch.i).Format(time.RFC3339)
}
