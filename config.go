package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// newConfLoader does IO immediately to validate the config file at the
// path. This is not ideal for all purposes, but works here for the following
// reasons. Since Watch being is called in a background goroutine, it finding an
// error in the config file will race with Let's Encrypt account creation. That
// means that every time it has to crash from the Watch going bad, we could be
// making a new LE account on the next boot. That's unkind and will get the
// process rate limited. But we also don't want the process to boot up in a
// state that is obviously invalid since people running this the first time
// might not know they screwed the config file up. So, take the L and load the
// config file here.
func newConfLoader(fp string) (*confLoader, error) {
	cl := &confLoader{
		path:       fp,
		lastCheck:  &unixEpoch{},
		lastChange: &unixEpoch{},
	}
	err := cl.load()
	if err != nil {
		return nil, err
	}
	return cl, nil
}

type confLoader struct {
	path       string
	lastCheck  *unixEpoch
	lastChange *unixEpoch
	lastHash   [sha256.Size]byte

	mu   sync.Mutex
	conf *allConf
}

func (cl *confLoader) Get() *allConf {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.conf
}

// Watch blocks
func (cl *confLoader) Watch() error {
	for {
		start := time.Now()
		err := cl.load()
		if err == errSameHash {
			continue
		}
		if err != nil {
			recordError(loadConfigStage, "unable to load config file in watch goroutine: %s", err)
			continue
		}
		cl.mu.Lock()
		next := start.Add(time.Duration(cl.conf.ConfigCheckInterval))
		cl.mu.Unlock()
		log.Printf("successfully loaded new config file. next check will be around %s", next)
		time.Sleep(next.Sub(start))
	}
	return errors.New("should never return")
}

var errSameHash = errors.New("same hash as last read config file")

func (cl *confLoader) load() error {
	cl.lastCheck.Set(time.Now().UnixNano())
	b, err := ioutil.ReadFile(cl.path)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if h == cl.lastHash {
		return errSameHash
	}

	conf, err := unmarshalConf(cl.path)
	if err != nil {
		return err
	}
	if err := validateConf(conf); err != nil {
		return err
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.conf = conf

	// lastHash is only used in this goroutine, and so doesn't need to be under
	// the lock. It's only here for clarity and to prevent setting the conf
	// without setting it.
	cl.lastHash = h

	cl.lastChange.Set(time.Now().UnixNano())
	return nil
}

type allConf struct {
	Email               string        `json:"email"`
	UseProd             *bool         `json:"use_prod"`
	AllowRemoteDebug    bool          `json:"allow_remote_debug"`
	Secrets             []*secretConf `json:"secrets"`
	TLSDir              string        `json:"tls_dir"`
	ConfigCheckInterval jsonDuration  `json:"config_check_interval"`
}

type secretConf struct {
	Namespace *string  `json:"namespace"`
	Name      string   `json:"name"`
	Domains   []string `json:"domains"`
	UseRSA    bool     `json:"use_rsa"` // use ECDSA if not set or if set to false, RSA for certs
}

func (sconf *secretConf) FullName() nsSecName {
	return nsSecName{*sconf.Namespace, sconf.Name}
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
		return "https://acme-v01.api.letsencrypt.org/directory"
	}
	return "https://acme-staging.api.letsencrypt.org/directory"
}

func unmarshalConf(fp string) (*allConf, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	conf := &allConf{}
	err = json.NewDecoder(f).Decode(conf)
	if err != nil {
		return nil, err
	}
	if conf.ConfigCheckInterval == jsonDuration(0) {
		conf.ConfigCheckInterval = jsonDuration(30 * time.Second)
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
		if secConf.Namespace == nil {
			return fmt.Errorf("no Namespace given for secret config at index %d in \"secrets\" (is allowed to be the empty string)", i)
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
