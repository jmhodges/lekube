package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"contrib.go.opencensus.io/exporter/stackdriver"
	"go.opencensus.io/examples/exporter"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"golang.org/x/oauth2/google"
	"golang.org/x/time/rate"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	kubeapi "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
)

var (
	confPath     = flag.String("conf", "", "path to required JSON config file described by https://github.com/jmhodges/lekube/#config-format")
	httpAddr     = flag.String("addr", ":10080", "address to boot the HTTP server on")
	httpsAddr    = flag.String("httpsAddr", ":10443", "address to boot the HTTPS server on")
	leTimeoutDur = flag.Duration("leTimeout", 30*time.Minute, "max time to spend fetching and creating a certificate (but not time spent fetching and storing secrets)")

	fetchLECertPrefix    = "stages/fetch-cert/"
	fetchLECertAttempts  = stats.Int64(fetchLECertPrefix+"attempts", "The number of attempts when fetching the Let's Encrypt certificate.", stats.UnitDimensionless)
	fetchLECertErrors    = stats.Int64(fetchLECertPrefix+"errors", "The number of errors when fetching the Let's Encrypt certificate.", stats.UnitDimensionless)
	fetchLECertSuccesses = stats.Int64(fetchLECertPrefix+"successes", "The number of successes when fetching the Let's Encrypt certificate.", stats.UnitDimensionless)

	fetchSecretPrefix    = "stages/fetch-secret/"
	fetchSecretAttempts  = stats.Int64(fetchSecretPrefix+"attempts", "The number of attempts when fetching a TLS k8s Secret.", stats.UnitDimensionless)
	fetchSecretErrors    = stats.Int64(fetchSecretPrefix+"errors", "The number of errors when fetching a TLS k8s Secret.", stats.UnitDimensionless)
	fetchSecretSuccesses = stats.Int64(fetchSecretPrefix+"successes", "The number of successes when fetching a TLS k8s Secret.", stats.UnitDimensionless)

	storeSecretPrefix    = "stages/store-secret/"
	storeSecretAttempts  = stats.Int64(storeSecretPrefix+"attempts", "The number of attempts when storing a TLS k8s Secret.", stats.UnitDimensionless)
	storeSecretErrors    = stats.Int64(storeSecretPrefix+"errors", "The number of errors when storing a TLS k8s Secret.", stats.UnitDimensionless)
	storeSecretSuccesses = stats.Int64(storeSecretPrefix+"successes", "The number of successes when storing a TLS k8s Secret.", stats.UnitDimensionless)
	storeSecretUpdates   = stats.Int64(storeSecretPrefix+"updates", "The number of times a TLS k8s Secret was stored with an Update verb.", stats.UnitDimensionless)
	storeSecretCreates   = stats.Int64(storeSecretPrefix+"creates", "The number of times a TLS k8s Secret was stored with a Create verb.", stats.UnitDimensionless)

	loadConfigPrefix    = "stages/load-config/"
	loadConfigAttempts  = stats.Int64(loadConfigPrefix+"attempts", "The number of attempts when loading the lekube config.", stats.UnitDimensionless)
	loadConfigErrors    = stats.Int64(loadConfigPrefix+"errors", "The number of errors when loading the lekube config.", stats.UnitDimensionless)
	loadConfigSuccesses = stats.Int64(loadConfigPrefix+"successes", "The number of successes when loading the lekube config.", stats.UnitDimensionless)

	runCount   = stats.Int64("runs", "The number of top-level runs lekube has made.", stats.UnitDimensionless)
	errorCount = stats.Int64("errors", "The number of top-level runs lekube has seen.", stats.UnitDimensionless)

	lastCheck  = stats.Int64("last-config-check", "The unix epoch time that the configuration file was checked for changes.", stats.UnitDimensionless)
	lastChange = stats.Int64("last-config-change", "The unix epoch time that the configuration file was reloaded because changes were found.", stats.UnitDimensionless)

	buildSHA = "<debug>"
)

func main() {
	flag.Parse()
	if *confPath == "" {
		log.Printf("-conf flag is required")
		flag.Usage()
		os.Exit(2)
	}

	usePrintTracer := os.Getenv("USE_PRINT_EXPORTER") != ""
	log.Println("USE_PRINT_EXPORTER is", usePrintTracer)
	if usePrintTracer {
		exporter := &exporter.PrintExporter{}
		view.RegisterExporter(exporter)
	}

	view.SetReportingPeriod(1 * time.Minute)
	if metadata.OnGCE() {
		// We don't want to load these checks because we want the dev
		// environment to work quietly and not crash. Plus, the errors
		// FindDefaultCredentials returns are undocumented and unexported in its
		// API.
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		creds, err := google.FindDefaultCredentials(ctx)
		if err != nil {
			fmt.Errorf("unable to find default Google credentials for tracing and metrics: %s", err)
		}
		projID, err := metadata.ProjectID()
		if err != nil {
			log.Fatalf("unable to get ProjectID from GCE metadata: %s", err)
		}
		location, err := metadata.InstanceAttributeValue("cluster-location")
		if err != nil {
			log.Fatalf("unable to get cluster-location InstanceAttributeValue from GCE metadata")
		}
		clusterName, err := metadata.InstanceAttributeValue("cluster-name")
		if err != nil {
			log.Fatalf("unable to get cluster-name InstanceAttributeValue from GCE emetadata")
		}
		exporter, err := stackdriver.NewExporter(stackdriver.Options{
			ProjectID:    creds.ProjectID,
			MetricPrefix: "lekube",
			Resource: &monitoredres.MonitoredResource{
				Type: "k8s_container",
				Labels: map[string]string{
					"project_id":     projID,
					"location":       location,
					"cluster_name":   clusterName,
					"namespace_name": os.Getenv("K8S_NAMESPACE"),
					"pod_name":       os.Getenv("K8S_POD"),
					"container_name": os.Getenv("K8S_CONTAINER"),
				},
			},
			OnError: func(err error) {
				log.Printf("stackdriver exporter saw error: %s", err)
			},
		})
		if err != nil {
			log.Fatalf("unable to create Stackdriver opencensus exporter: %s", err)
		}
		view.RegisterExporter(exporter)
	}

	statViews := countViews(
		fetchSecretAttempts,
		fetchSecretErrors,
		fetchSecretSuccesses,
		storeSecretAttempts,
		storeSecretErrors,
		storeSecretSuccesses,
		storeSecretUpdates,
		storeSecretCreates,
		loadConfigAttempts,
		loadConfigErrors,
		loadConfigSuccesses,
		runCount,
		errorCount,
	)

	statViews = append(statViews, latestViews(lastCheck, lastChange)...)
	if err := view.Register(statViews...); err != nil {
		log.Fatalf("unable to register the opencensus stat views: %s", err)
	}
	// FIXME for adding to the alerts
	ctx2 := context.Background()
	stats.Record(errorCount.M(ctx2, 1))
	cLoader, conf, err := newConfLoader(*confPath, lastCheck, lastChange)
	if err != nil {
		log.Fatalf("unable to load configuration: %s", err)
	}

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

	kubeClient := k8s.NewForConfigOrDie(restConfig).CoreV1()

	limit := rate.NewLimiter(rate.Limit(3), 3)
	lcm := newLEClientMaker(httpClient, accountKey, responder, limit)
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

func run(lcm *leClientMaker, client corev1.CoreV1Interface, conf *allConf, leTimeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), leTimeout+20*time.Second)
	defer cancel()
	stats.Record(ctx, runCount.M(1))
	lcm.responder.Reset()
	tlsSecs := make(map[nsSecName]*tlsSecret)
	okaySecs := []*secretConf{}
	for _, secConf := range conf.Secrets {
		log.Printf("Fetching kubernetes secret %s", secConf.FullName())
		stats.Record(ctx, fetchSecretAttempts.M(1))
		tlsSec, err := fetchK8SSecret(client.Secrets(*secConf.Namespace), secConf.Name)
		if err != nil {
			recordError(fetchSecStage, "unable to fetch TLS secret value %#v: %s", secConf.Name, err)
			continue
		}
		stats.Record(ctx, fetchSecretSuccesses.M(1))
		log.Printf("Fetched kubernetes secret %s", secConf.FullName())

		tlsSecs[secConf.FullName()] = tlsSec
		okaySecs = append(okaySecs, secConf)
	}

	for _, secConf := range okaySecs {
		log.Printf("checking on %s", secConf.FullName())
		tlsSec := tlsSecs[secConf.FullName()]
		refreshCert := false
		if tlsSec == nil {
			log.Printf("no such secret %s", secConf.FullName())
			refreshCert = true
		} else if tlsSec.Cert == nil {
			log.Printf("no tls.crt in secret %s", secConf.FullName())
			refreshCert = true
		} else if closeToExpiration(tlsSec.Cert, time.Duration(conf.StartRenewDur)) {
			log.Printf("cert close to expiration in secret %s, NotAfter: %s; Now: %s StartRenewDur: %s", secConf.FullName(), tlsSec.Cert.NotAfter, time.Now(), time.Duration(conf.StartRenewDur))
			refreshCert = true
		} else if domainMismatch(tlsSec.Cert, secConf.Domains) {
			log.Printf("domain mismatch between cert and secret %s", secConf.FullName())
			refreshCert = true
		}

		if refreshCert {
			log.Printf("working on %s", secConf.FullName())
			workOn(ctx, tlsSec, secConf, lcm, client, conf, leTimeout)
		} else {
			log.Printf("no work needed for secret %s", secConf.FullName())
		}
	}
}

func workOn(ctx context.Context, tlsSec *tlsSecret, secConf *secretConf, lcm *leClientMaker, client corev1.CoreV1Interface, conf *allConf, leTimeout time.Duration) {
	stats.Record(ctx, fetchLECertAttempts.M(1))
	acmeClient, err := lcm.Make(ctx, dirURLFromConf(conf), conf.Email)
	if err != nil {
		recordError(fetchLECertStage, "unable to get client for Let's Encrypt API that is up to date: %s", err)
		return
	}
	leCert, err := acmeClient.CreateCert(ctx, secConf)
	if err != nil {
		recordError(fetchLECertStage, "unable to get Let's Encrypt certificate for %s: %s", secConf.FullName(), err)
		return
	}
	stats.Record(ctx, fetchLECertSuccesses.M(1))
	log.Printf("have new cert for %s", secConf.FullName())
	var oldSec *kubeapi.Secret
	if tlsSec != nil {
		oldSec = tlsSec.Secret
	}

	stats.Record(ctx, storeSecretAttempts.M(1))
	err = storeK8SSecret(ctx, client.Secrets(*secConf.Namespace), secConf, oldSec, leCert)
	if err != nil {
		recordError(storeSecStage, "unable to store the TLS cert and key as secret %#v: %s", secConf.Name, err)
		return
	}
	stats.Record(ctx, storeSecretSuccesses.M(1))
	log.Printf("successfully stored new cert in %s", secConf.FullName())
}

// fetchK8SSecret may return a nil tlsSecret if no secret was found.
func fetchK8SSecret(client corev1.SecretInterface, secretName string) (*tlsSecret, error) {
	sec, err := client.Get(secretName, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
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

func storeK8SSecret(ctx context.Context, cl corev1.SecretInterface, secConf *secretConf, oldSec *kubeapi.Secret, leCert *newCert) error {
	f := cl.Update
	sec := oldSec
	if oldSec == nil {
		f = cl.Create
		sec = &kubeapi.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: secConf.Name,
			},
			Data: make(map[string][]byte),
		}
		stats.Record(ctx, storeSecretCreates.M(1))
	} else {
		stats.Record(ctx, storeSecretUpdates.M(1))
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

var stageErrors = map[stage]*stats.Int64Measure{
	fetchSecStage:    fetchSecretErrors,
	fetchLECertStage: fetchLECertErrors,
	storeSecStage:    storeSecretErrors,
	loadConfigStage:  loadConfigErrors,
}

func recordError(st stage, format string, args ...interface{}) {
	// Any context we pass in here might have already expired, so we create a
	// new one just for the stats.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	stats.Record(ctx, errorCount.M(1), stageErrors[st].M(1))
	log.Printf(format, args...)
}

func closeToExpiration(cert *x509.Certificate, startRenewDur time.Duration) bool {
	t := time.Now().Add(startRenewDur)
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

func countViews(measures ...*stats.Int64Measure) []*view.View {
	out := make([]*view.View, len(measures))
	for i, m := range measures {
		out[i] = &view.View{
			Name:        m.Name(),
			Description: m.Description(),
			Measure:     m,
			Aggregation: view.Count(),
		}
	}
	return out
}

func latestViews(measures ...*stats.Int64Measure) []*view.View {
	out := make([]*view.View, len(measures))
	for i, m := range measures {
		out[i] = &view.View{
			Name:        m.Name(),
			Description: m.Description(),
			Measure:     m,
			Aggregation: view.LastValue(),
		}
	}
	return out
}
