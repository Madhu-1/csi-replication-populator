/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"os"

	controller "github.com/Madhu-1/csi-replication-populator/controller"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

var version = "unknown"

func main() {
	var (
		masterURL   string
		kubeconfig  string
		showVersion bool
	)
	klog.InitFlags(nil)
	// Controller args
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	// Other args
	flag.BoolVar(&showVersion, "version", false, "display the version string")
	flag.Parse()

	if showVersion {
		fmt.Println(os.Args[0], version)
		os.Exit(0)
	}

	const (
		groupName  = "replication.storage.openshift.io"
		apiVersion = "v1alpha1"
		kind       = "VolumeReplication"
		resource   = "volumereplications"
	)
	var (
		gk  = schema.GroupKind{Group: groupName, Kind: kind}
		gvr = schema.GroupVersionResource{Group: groupName, Version: apiVersion, Resource: resource}
	)
	controller.RunController(masterURL, kubeconfig, gk, gvr)
}
