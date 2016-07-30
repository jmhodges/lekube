package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

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
	mu   sync.Mutex
	path string
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
	for {
		select {
		case <-w.Event:
			log.Printf("caught config file event (%s), reloading it")
			cl.load()
		}
	}
	return errors.New("should never return")
}

func (cl *confLoader) load() error {
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
	return nil
}

func validateConf(conf *allConf) error {
	if conf.Email == "" {
		fmt.Errorf("'email' must be set in the config file %#v", *confPath)
	}
	secs := make(map[nsSecName]bool)
	for i, secConf := range conf.Secrets {
		if secConf.Name == "" {
			fmt.Errorf("no Name given for secret config at index %d in \"secrets\"", i)
		}
		if secConf.Namespace == nil {
			fmt.Errorf("no Namespace given for secret config at index %d in \"secrets\"", i)
		}
		name := secConf.FullName()
		if secs[name] {
			fmt.Errorf("duplicate config for secret %s", secConf.Name)
		}
		secs[name] = true
		if len(secConf.Domains) == 0 {
			fmt.Errorf("no domains given for secret %s", secConf.Name)
		}
		for j, d := range secConf.Domains {
			d = strings.TrimSpace(d)
			if d == "" {
				fmt.Errorf("empty string in domains of secret config at index %d in \"secrets\"", j)
			}
			secConf.Domains[j] = d
		}
	}
	return nil
}
