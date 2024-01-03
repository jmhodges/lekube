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
	"sync/atomic"
	"time"

	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/contrib/detectors/gcp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"golang.org/x/time/rate"
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

	tracer = otel.Tracer("lekube")
	meter  = otel.Meter("lekube")

	fetchLECertPrefix    = "stages/fetch-cert/"
	fetchLECertAttempts  = mustInt64Counter(fetchLECertPrefix+"attempts", "The number of attempts when fetching the Let's Encrypt certificate.")
	fetchLECertErrors    = mustInt64Counter(fetchLECertPrefix+"errors", "The number of errors when fetching the Let's Encrypt certificate.")
	fetchLECertSuccesses = mustInt64Counter(fetchLECertPrefix+"successes", "The number of successes when fetching the Let's Encrypt certificate.")

	fetchSecretPrefix    = "stages/fetch-secret/"
	fetchSecretAttempts  = mustInt64Counter(fetchSecretPrefix+"attempts", "The number of attempts when fetching a TLS k8s Secret.")
	fetchSecretErrors    = mustInt64Counter(fetchSecretPrefix+"errors", "The number of errors when fetching a TLS k8s Secret.")
	fetchSecretSuccesses = mustInt64Counter(fetchSecretPrefix+"successes", "The number of successes when fetching a TLS k8s Secret.")

	storeSecretPrefix    = "stages/store-secret/"
	storeSecretAttempts  = mustInt64Counter(storeSecretPrefix+"attempts", "The number of attempts when storing a TLS k8s Secret.")
	storeSecretErrors    = mustInt64Counter(storeSecretPrefix+"errors", "The number of errors when storing a TLS k8s Secret.")
	storeSecretSuccesses = mustInt64Counter(storeSecretPrefix+"successes", "The number of successes when storing a TLS k8s Secret.")
	storeSecretUpdates   = mustInt64Counter(storeSecretPrefix+"updates", "The number of times a TLS k8s Secret was stored with an Update verb.")
	storeSecretCreates   = mustInt64Counter(storeSecretPrefix+"creates", "The number of times a TLS k8s Secret was stored with a Create verb.")

	loadConfigPrefix    = "stages/load-config/"
	loadConfigAttempts  = mustInt64Counter(loadConfigPrefix+"attempts", "The number of attempts when loading the lekube config.")
	loadConfigErrors    = mustInt64Counter(loadConfigPrefix+"errors", "The number of errors when loading the lekube config.")
	loadConfigSuccesses = mustInt64Counter(loadConfigPrefix+"successes", "The number of successes when loading the lekube config.")

	runStartsCount   = mustInt64Counter("run-starts", "The number of top-level runs lekube has started.")
	runFinishesCount = mustInt64Counter("run-finishes", "The number of top-level runs lekube has finished.")
	errorCount       = mustInt64Counter("errors", "The number of top-level runs lekube has seen.")

	lastCheck, _  = mustInt64Gauge("last-config-check", "The unix epoch time that the configuration file was checked for changes.")
	lastChange, _ = mustInt64Gauge("last-config-change", "The unix epoch time that the configuration file was reloaded because changes were found.")

	buildSHA = "<debug>"
)

func main() {
	flag.Parse()
	if *confPath == "" {
		log.Printf("-conf flag is required")
		flag.Usage()
		os.Exit(2)
	}

	// bootTimeCtx is a Context that should only be used during initial startup
	// of the lekube process. It's not exactly how long we allow lekube to boot
	// (the k8s deployment's timeout is likely lower), and it doesn't gate some
	// of the http Server boots, but it sets a a good upper bound for
	// initializing our metrics and similar.
	bootTimeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	usePrintTracer := os.Getenv("USE_PRINT_EXPORTER") != ""
	log.Println("USE_PRINT_EXPORTER is", usePrintTracer)
	traceProviderOpts := []sdktrace.TracerProviderOption{}
	metricProviderOpts := []sdkmetric.Option{}
	if usePrintTracer {
		traceExporter, err := stdouttrace.New()
		if err != nil {
			log.Fatalf("unable to create OpenTelemetry stdout tracer: %v", err)
		}
		metricExporter, err := stdoutmetric.New()
		if err != nil {
			log.Fatalf("unable to create OpenTelemetry stdout metric exporter: %v", err)
		}
		traceProviderOpts = append(traceProviderOpts, sdktrace.WithBatcher(traceExporter))
		metricProviderOpts = append(metricProviderOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)))
	}

	traceExporter, err := texporter.New()
	if err != nil {
		log.Fatalf("unable to create GCP OpenTelemetry trace exporter: %v", err)
	}
	metricExporter, err := mexporter.New()
	if err != nil {
		log.Fatalf("unable to create GCP OpenTelemetry metric exporter: %v", err)
	}
	metricReader := sdkmetric.NewPeriodicReader(metricExporter)

	resource, err := resource.New(bootTimeCtx,
		// Use the GCP resource detector to detect information about the GCP platform
		resource.WithDetectors(gcp.NewDetector()),
		// Keep the default detectors
		resource.WithTelemetrySDK(),
		// Add your own custom attributes to identify your application
		resource.WithAttributes(
			semconv.ServiceName("lekube"),
		),
	)
	if err != nil {
		log.Fatalf("unable to create GCP OpenTelemetry resource mapping: %s", err)
	}
	traceProviderOpts = append(traceProviderOpts, sdktrace.WithResource(resource), sdktrace.WithBatcher(traceExporter))
	metricProviderOpts = append(metricProviderOpts, sdkmetric.WithResource(resource), sdkmetric.WithReader(metricReader))

	tp := sdktrace.NewTracerProvider(traceProviderOpts...)
	mp := sdkmetric.NewMeterProvider(metricProviderOpts...)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	defer tp.Shutdown(context.Background())
	defer mp.Shutdown(context.Background())

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

	_, err = lcm.Make(bootTimeCtx, dirURLFromConf(conf), conf.Email)
	if err != nil {
		log.Fatalf("unable to make an account with %s using email %s: %s", dirURLFromConf(conf), conf.Email, err)
	}

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

	m.Handle("/", otelhttp.NewHandler(responder, "leresponder"))

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

func mustInt64Counter(name, description string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		log.Fatalf("mustInt64Counter failed for name: %#v; description: %#v: %s", name, description, err)
	}
	return c
}

func mustInt64Gauge(name, description string) (*atomic.Int64, metric.Int64ObservableGauge) {
	rawGauge := new(atomic.Int64)
	g, err := meter.Int64ObservableGauge(name, metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
		obs.Observe(rawGauge.Load())
		return nil
	}))
	if err != nil {
		log.Fatalf("mustInt64Gauge failed for name: %#v; description: %#v: %s", name, description, err)
	}
	return rawGauge, g
}

func run(lcm *leClientMaker, client corev1.CoreV1Interface, conf *allConf, leTimeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), leTimeout+20*time.Second)
	defer cancel()
	ctx, span := tracer.Start(ctx, "lekube/run")
	defer span.End()
	runStartsCount.Add(ctx, 1)
	defer runFinishesCount.Add(ctx, 1)

	lcm.responder.Reset()
	tlsSecs := make(map[nsSecName]*tlsSecret)
	okaySecs := []*secretConf{}

	fetchCtx, fetchSpan := tracer.Start(ctx, "fetch-secrets")
	fetchAttempts := 0
	fetchErrors := 0
	fetchSuccesses := 0
	for _, secConf := range conf.Secrets {
		secCtx, secSpan := tracer.Start(fetchCtx, "fetch-secret")
		log.Printf("Fetching kubernetes secret %s", secConf.FullName())
		fetchSecretAttempts.Add(secCtx, 1)
		secSpan.SetAttributes(attribute.String("secret.name", secConf.Name), attribute.String("secret.namespace", secConf.Namespace))
		tlsSec, err := fetchK8SSecret(secCtx, client.Secrets(secConf.Namespace), secConf.Name)
		if err != nil {
			secSpan.SetStatus(codes.Error, err.Error())
			recordErrorMetric(fetchSecStage, "unable to fetch TLS secret value %#v: %s", secConf.Name, err)
			continue
		}
		secSpan.SetStatus(codes.Ok, "")
		fetchSecretSuccesses.Add(secCtx, 1)
		log.Printf("Fetched kubernetes secret %s", secConf.FullName())

		tlsSecs[secConf.FullName()] = tlsSec
		okaySecs = append(okaySecs, secConf)
	}
	fetchSpan.SetAttributes(attribute.Int("given", len(conf.Secrets)), attribute.Int("attempts", fetchAttempts), attribute.Int("errors", fetchErrors), attribute.Int("successes", fetchSuccesses))
	fetchSpan.End()

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
		} else if isRevokedLetsEncrypt(tlsSec.Cert) {
			log.Printf("Let's Encrypt revoked cert from their ALPN-01 bug in 2022-01")
			refreshCert = true
		} else if certPublicKeyAlgoDoesntMatch(tlsSec.Cert, secConf) {
			log.Printf("Requested key type (UseRSA: %t) doesn't match type of cert in secret %s", secConf.UseRSA, secConf.FullName())
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
	fetchCtx, fetchSpan := tracer.Start(ctx, "fetch-certs")
	defer fetchSpan.End()
	fetchSpan.SetAttributes(attribute.String("secret.name", secConf.Name), attribute.String("secret.namespace", secConf.Namespace))
	fetchLECertAttempts.Add(fetchCtx, 1)

	acmeClient, err := lcm.Make(fetchCtx, dirURLFromConf(conf), conf.Email)
	if err != nil {
		fetchSpan.SetStatus(codes.Error, fmt.Sprintf("unable to get client for Let's Encrypt API that is up to date: %s", err))
		recordErrorMetric(fetchLECertStage, "unable to get client for Let's Encrypt API that is up to date: %s", err)
		return
	}
	leCert, err := acmeClient.CreateCert(fetchCtx, secConf)
	if err != nil {
		fetchSpan.SetStatus(codes.Error, fmt.Sprintf("unable to get Let's Encrypt certificate: %s", err))
		recordErrorMetric(fetchLECertStage, "unable to get Let's Encrypt certificate for %s: %s", secConf.FullName(), err)
		return
	}
	fetchLECertSuccesses.Add(fetchCtx, 1)
	log.Printf("have new cert for %s", secConf.FullName())
	var oldSec *kubeapi.Secret
	if tlsSec != nil {
		oldSec = tlsSec.Secret
	}
	fetchSpan.End()

	storeCtx, storeSpan := tracer.Start(ctx, "store-secrets")
	defer storeSpan.End()
	storeSpan.SetAttributes(attribute.String("secret.name", secConf.Name), attribute.String("secret.namespace", secConf.Namespace))
	storeSecretAttempts.Add(storeCtx, 1)
	err = storeK8SSecret(ctx, client.Secrets(secConf.Namespace), secConf, oldSec, leCert)
	if err != nil {
		storeSpan.SetStatus(codes.Error, err.Error())
		recordErrorMetric(storeSecStage, "unable to store the TLS cert and key as secret %#v: %s", secConf.Name, err)
		return
	}
	storeSpan.SetStatus(codes.Ok, "")
	storeSecretSuccesses.Add(storeCtx, 1)
	log.Printf("successfully stored new cert in %s", secConf.FullName())
}

// fetchK8SSecret may return a nil tlsSecret if no secret was found.
func fetchK8SSecret(ctx context.Context, client corev1.SecretInterface, secretName string) (*tlsSecret, error) {
	sec, err := client.Get(ctx, secretName, metav1.GetOptions{})
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
	if oldSec == nil {
		sec := &kubeapi.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: secConf.Name,
			},
			Data: make(map[string][]byte),
		}
		sec.Data["tls.crt"] = leCert.Cert
		sec.Data["tls.key"] = leCert.Key

		storeSecretCreates.Add(ctx, 1)
		_, err := cl.Create(ctx, sec, metav1.CreateOptions{})
		return err
	}

	sec := oldSec.DeepCopy()
	sec.Data["tls.crt"] = leCert.Cert
	sec.Data["tls.key"] = leCert.Key

	storeSecretUpdates.Add(ctx, 1)
	_, err := cl.Update(ctx, sec, metav1.UpdateOptions{})

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

var stageErrors = map[stage]metric.Int64Counter{
	fetchSecStage:    fetchSecretErrors,
	fetchLECertStage: fetchLECertErrors,
	storeSecStage:    storeSecretErrors,
	loadConfigStage:  loadConfigErrors,
}

func recordErrorMetric(st stage, format string, args ...interface{}) {
	// v1.21.0 and earlier of OpenTelemetry have a bug where they drop metrics
	// that are added with a context that has been errored. This is unfortunate
	// because it means timeouts and other Context cancelling errors will never
	// be recorded. This will be fixed when
	// https://github.com/open-telemetry/opentelemetry-go/commit/8e756513a630cc0e80c8b65528f27161a87a3cc8
	// is released, but that's not yet. Until then, we'll set up a context on
	// our own here. When dependabot bumps the otel version, our
	// TestOtelDroppingContextData will fail. When that happens, we can change
	// this func to accept a Context, fix the context.TODOs elsewhere, and
	// remove the test.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	errorCount.Add(ctx, 1)
	stageErrors[st].Add(ctx, 1)
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

// https://community.letsencrypt.org/t/2022-01-25-issue-with-tls-alpn-01-validation-method/170450
var letsEncryptFixDeployTime = time.Date(2022, time.January, 26, 00, 48, 0, 0, time.UTC)

// isRevokedLetsEncrypt returns whether the certificate is likely to be part of
// a batch of certificates revoked by Let's Encrypt in January 2022. This check
// can be safely removed from May 2022.
func isRevokedLetsEncrypt(cert *x509.Certificate) bool {
	O := cert.Issuer.Organization
	return len(O) == 1 && O[0] == "Let's Encrypt" &&
		cert.NotBefore.Before(letsEncryptFixDeployTime)
}

// certPublicKeyAlgoDoesntMatch returns true if the type of key (RSA or ECDSA) used to
// generate the existing certificate differs from the type requested.
func certPublicKeyAlgoDoesntMatch(cert *x509.Certificate, secConf *secretConf) bool {
	// If you adjust this UseRSA code, be sure to also adjust the use of UseRSA
	// in the Let's Encrypt code.
	if secConf.UseRSA {
		return cert.PublicKeyAlgorithm != x509.RSA
	} else {
		return cert.PublicKeyAlgorithm != x509.ECDSA
	}
}
