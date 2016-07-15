package main

import "testing"

func TestGoldenPath(t *testing.T) {
	if err := startMinikube(); err != nil {
		t.Fatalf("unable to start minikube: %s", err)
	}
	defer stopMinikube()
	client := newKubeClient()
	keyPair := newKeyPair()
	secName := "good"
	if err := client.CreateSecret(secName, keyPair); err != nil {
		t.Fatalf("unable to create secret %#v: %s", secName, err)
	}
	// FIXME don't forget the -server flag
	if err := createKubeResources(goldenpath); err != nil {
		t.Fatalf("unable to create kubernetes resources")
	}
	if err := healthcheckResources(); err != nil {
		t.Fatalf("kubernetes resources didn't healthcheck in time")
	}
}

var goldenpath = []interface{}{}
