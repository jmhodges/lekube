package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/howeyc/fsnotify"
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
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	err = w.Watch(cl.path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(cl.path)
	err = w.Watch(dir)
	if err != nil {
		return err
	}
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

func validateConf(conf *allConf) error {
	if conf.Email == "" {
		return fmt.Errorf("'email' must be set in the config file %#v", *confPath)
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
