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

func newConfLoader(fp string) (*confLoader, error) {
	cl := &confLoader{path: fp}
	err := cl.load()
	if err != nil {
		return nil, err
	}
	return cl, nil
}

type confLoader struct {
	path     string
	lastHash [sha256.Size]byte

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
	tickDur := 5 * time.Minute
	tick := time.NewTicker(tickDur)
	for range tick.C {
		t := time.Now()
		err := cl.load()
		if err == errSameHash {
			continue
		}
		if err != nil {
			recordError(loadConfigStage, "unable to load config file in watch goroutine: %s", err)
			continue
		}
		t = t.Add(tickDur)
		log.Printf("successfully loaded new config file. next check will be around around %s")
	}
	return errors.New("should never return")
}

var errSameHash = errors.New("same hash as last read config file")

func (cl *confLoader) load() error {
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
	return nil
}

type allConf struct {
	Email   string        `json:"email"`
	UseProd *bool         `json:"use_prod"`
	Secrets []*secretConf `json:"secrets"`
}

type secretConf struct {
	Namespace *string  `json:"namespace"`
	Name      string   `json:"name"`
	Domains   []string `json:"domains"`
	UseRSA    bool     // use ECDSA if not set or if set to false, RSA for certs
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
	return conf, err
}

func validateConf(conf *allConf) error {
	if conf.Email == "" {
		return fmt.Errorf("'email' must be set in the config file %#v", *confPath)
	}

	if conf.UseProd == nil {
		return fmt.Errorf("'use_prod' must be set to `false` (to use the staging Let's Encrypt API with untrusted certs and higher rate limits), or `true` (to use the production Let's Encrypt API with working certs but much lower rate limits. lekube strongly recommends setting this to `false` until you've seen your staging certs be successfully created.")
	}

	secs := make(map[nsSecName]bool)
	for i, secConf := range conf.Secrets {
		if secConf.Name == "" {
			return fmt.Errorf("no Name given for secret config at index %d in \"secrets\"", i)
		}
		if secConf.Namespace == nil {
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
