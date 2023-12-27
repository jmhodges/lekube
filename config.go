package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// newConfLoader does I/O immediately to validate the config file at the given
// path and return its *allConf representation. Doing I/O in a constructor is
// not ideal for all purposes, but works here for the following reasons. Since
// Watch only returns if the config file changes without error, finding an error
// in the config file using it alone is impossible. That means that lekube can
// boot with a busted config file and the user wouldn't know unless they were
// tracking the stats. But changing Watch to always return the error would mean
// the lekube process could cratch whenever the user writes a busted config into
// that file path and that would cause a reboot and another account
// acquisition. That's a bummer. So, take the L and load the config file here
// and let Watch eat and record the errors.
func newConfLoader(fp string, lastCheck, lastChange *atomic.Int64) (*confLoader, *allConf, error) {
	cl := &confLoader{
		path:       fp,
		lastCheck:  lastCheck,
		lastChange: lastChange,
	}
	err := cl.load()
	if err != nil {
		loadConfigErrors.Add(context.TODO(), 1)
		return nil, nil, err
	}
	loadConfigSuccesses.Add(context.TODO(), 1)
	return cl, cl.Get(), nil
}

type confLoader struct {
	path      string
	lastCheck *atomic.Int64

	// loadMu locks calls to confLoader.load, but doesn't prevent concurrent
	// reads of confLoader.conf (that's handled by confMu). This allows us to
	// prevent multiple reads of the config file on disk without blocking calls
	// to confloader.Get on file I/O.
	loadMu sync.Mutex

	// confMu locks the writes and reads of lastChange, lastHash, and, most
	// imporantly, the conf. Locking lastHash (while locking all of load
	// separately) prevents concurrent Watches from reading different versions
	// of the file and one with and older version setting the conf at a later
	// time than the others.
	confMu     sync.Mutex
	lastChange *atomic.Int64
	lastHash   [sha256.Size]byte
	conf       *allConf
}

// FIXME make it return the struct, for race condition reasons.
func (cl *confLoader) Get() *allConf {
	cl.confMu.Lock()
	defer cl.confMu.Unlock()
	return cl.conf
}

// Watch blocks until a change in the config is seen and succesfully validates. If
// the config cannot be read or it does not parse or validate, it is not
// returned and Watch continues to block.
func (cl *confLoader) Watch() *allConf {
	var prevErr error
	for {
		loadConfigAttempts.Add(context.TODO(), 1)
		start := time.Now()
		err := cl.load()
		c := cl.Get()
		if err == nil {
			if prevErr != nil {
				log.Printf("previous config file error resolved and load was successful")
			}
			prevErr = nil
			loadConfigSuccesses.Add(context.TODO(), 1)
			return c
		}

		waitDur := 30 * time.Second
		// c is always non-nil here since we require the first load of the
		// config to occur at construction time in newConfLoader. We might
		// have a c from a previous load, but it'll be useful.
		waitDur = time.Duration(c.ConfigCheckInterval)
		next := start.Add(waitDur)

		prevLoadSuccessful := prevErr == nil
		if err == errSameHash {
			if prevLoadSuccessful {
				// If the last load where the config had actually changed was
				// successful, then the good conf remained in place in this load
				// and we can record it as a success.
				loadConfigSuccesses.Add(context.TODO(), 1)
			} else {
				// If the last load where the config had actually changed was in
				// error, then the bad conf remained, so we can record this load
				// as an error. However, we don't want the logs consumed
				// entirely with repeated error messages, so just increment the
				// stat.
				loadConfigErrors.Add(context.TODO(), 1)
			}
		} else {
			prevErr = err
			recordErrorMetric(loadConfigStage, "unable to load config file in watch goroutine: %s", err)
		}
		time.Sleep(next.Sub(start))
	}
}

var errSameHash = errors.New("same hash as last read config file")

func (cl *confLoader) load() error {
	cl.loadMu.Lock()
	defer cl.loadMu.Unlock()

	cl.lastCheck.Store(time.Now().UnixNano())
	b, err := os.ReadFile(cl.path)
	if err != nil {
		return err
	}

	cl.confMu.Lock()
	defer cl.confMu.Unlock()

	h := sha256.Sum256(b)
	if h == cl.lastHash {
		return errSameHash
	}

	conf, err := unmarshalConf(b)
	if err != nil {
		return err
	}
	if err := validateConf(conf); err != nil {
		return err
	}

	cl.conf = conf
	cl.lastHash = h
	cl.lastChange.Store(time.Now().UnixNano())
	return nil
}

type allConf struct {
	Email               string        `json:"email"`
	UseProd             *bool         `json:"use_prod"`
	AllowRemoteDebug    bool          `json:"allow_remote_debug"`
	Secrets             []*secretConf `json:"secrets"`
	TLSDir              string        `json:"tls_dir"`
	ConfigCheckInterval jsonDuration  `json:"config_check_interval"`
	StartRenewDur       jsonDuration  `json:"start_renew_duration"`
}

type secretConf struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Domains   []string `json:"domains"`
	UseRSA    bool     `json:"use_rsa"` // use ECDSA if not set or if set to false, RSA for certs
}

func (sconf *secretConf) FullName() nsSecName {
	return nsSecName{sconf.Namespace, sconf.Name}
}

type nsSecName struct {
	ns   string
	name string
}

func (n nsSecName) String() string {
	return fmt.Sprintf("%s:%s", n.ns, n.name)
}

// ErrDurationMustBeString is returned when a non-string value is
// presented to be deserialized as a ConfigDuration
var ErrDurationMustBeString = errors.New("cannot JSON unmarshal something other than a string into a ConfigDuration")

type jsonDuration time.Duration

// UnmarshalJSON parses a string into a ConfigDuration using
// time.ParseDuration.  If the input does not unmarshal as a
// string, then UnmarshalJSON returns ErrDurationMustBeString.
func (d *jsonDuration) UnmarshalJSON(b []byte) error {
	s := ""
	err := json.Unmarshal(b, &s)
	if err != nil {
		if _, ok := err.(*json.UnmarshalTypeError); ok {
			return ErrDurationMustBeString
		}
		return err
	}
	dd, err := time.ParseDuration(s)
	*d = jsonDuration(dd)
	return err
}

func (d jsonDuration) String() string {
	return time.Duration(d).String()
}

func dirURLFromConf(conf *allConf) string {
	if *conf.UseProd {
		return "https://acme-v02.api.letsencrypt.org/directory"
	}
	return "https://acme-staging-v02.api.letsencrypt.org/directory"
}

func unmarshalConf(jsonData []byte) (*allConf, error) {
	conf := &allConf{}
	err := json.Unmarshal(jsonData, conf)
	if err != nil {
		return nil, err
	}
	if conf.ConfigCheckInterval == jsonDuration(0) {
		conf.ConfigCheckInterval = jsonDuration(30 * time.Second)
	}
	if conf.StartRenewDur == jsonDuration(0) {
		conf.StartRenewDur = jsonDuration(3 * 7 * 24 * time.Hour)
	}
	return conf, err
}

func validateConf(conf *allConf) error {
	if conf.Email == "" {
		return fmt.Errorf("'email' must be set in the config file %#v", *confPath)
	}

	if conf.UseProd == nil {
		return fmt.Errorf("'use_prod' must be set to `false` or `true`. `false will mean use the staging Let's Encrypt API (which has untrusted certs and higher rate limits), and `true` means use the production Let's Encrypt API with working certs but much lower rate limits. lekube strongly recommends setting this to `false` until you've seen your staging certs be successfully created.")
	}

	secs := make(map[nsSecName]bool)
	for i, secConf := range conf.Secrets {
		if secConf.Name == "" {
			return fmt.Errorf("no Name given for secret config at index %d in \"secrets\"", i)
		}
		if secConf.Namespace == "" {
			return fmt.Errorf("no Namespace given for secret config at index %d in \"secrets\"", i)
		}
		name := secConf.FullName()
		if secs[name] {
			return fmt.Errorf("duplicate config for secret %s", secConf.Name)
		}
		secs[name] = true
		if len(secConf.Domains) == 0 {
			return fmt.Errorf("no domains given for secret %s", secConf.Name)
		}
		for j, d := range secConf.Domains {
			d = strings.TrimSpace(d)
			if d == "" {
				return fmt.Errorf("empty string in domains of secret config at index %d in \"secrets\"", j)
			}
			secConf.Domains[j] = d
		}
	}
	return nil
}
