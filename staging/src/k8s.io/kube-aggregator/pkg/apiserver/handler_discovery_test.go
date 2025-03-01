/*
Copyright 2022 The Kubernetes Authors.

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

package apiserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	fuzz "github.com/google/gofuzz"
	"github.com/stretchr/testify/require"
	apidiscoveryv2beta1 "k8s.io/api/apidiscovery/v2beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	discoveryendpoint "k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
	scheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/kube-aggregator/pkg/apiserver"
)

// Test that the discovery manager starts and aggregates from two local API services
func TestBasic(t *testing.T) {
	service1 := discoveryendpoint.NewResourceManager()
	service2 := discoveryendpoint.NewResourceManager()
	apiGroup1 := fuzzAPIGroups(2, 5, 25)
	apiGroup2 := fuzzAPIGroups(2, 5, 50)
	service1.SetGroups(apiGroup1.Items)
	service2.SetGroups(apiGroup2.Items)
	aggregatedResourceManager := discoveryendpoint.NewResourceManager()
	aggregatedManager := apiserver.NewDiscoveryManager(aggregatedResourceManager)

	for _, g := range apiGroup1.Items {
		for _, v := range g.Versions {
			aggregatedManager.AddAPIService(&apiregistrationv1.APIService{
				ObjectMeta: metav1.ObjectMeta{
					Name: v.Version + "." + g.Name,
				},
				Spec: apiregistrationv1.APIServiceSpec{
					Group:   g.Name,
					Version: v.Version,
					Service: &apiregistrationv1.ServiceReference{
						Name: "service1",
					},
				},
			}, service1)
		}
	}

	for _, g := range apiGroup2.Items {
		for _, v := range g.Versions {
			aggregatedManager.AddAPIService(&apiregistrationv1.APIService{
				ObjectMeta: metav1.ObjectMeta{
					Name: v.Version + "." + g.Name,
				},
				Spec: apiregistrationv1.APIServiceSpec{
					Group:   g.Name,
					Version: v.Version,
					Service: &apiregistrationv1.ServiceReference{
						Name: "service2",
					},
				},
			}, service2)
		}
	}

	testCtx, _ := context.WithCancel(context.Background())
	go aggregatedManager.Run(testCtx.Done())

	cache.WaitForCacheSync(testCtx.Done(), aggregatedManager.ExternalServicesSynced)

	response, _, parsed := fetchPath(aggregatedResourceManager, "")
	if response.StatusCode != 200 {
		t.Fatalf("unexpected status code %d", response.StatusCode)
	}
	checkAPIGroups(t, apiGroup1, parsed)
	checkAPIGroups(t, apiGroup2, parsed)
}

func checkAPIGroups(t *testing.T, api apidiscoveryv2beta1.APIGroupDiscoveryList, response *apidiscoveryv2beta1.APIGroupDiscoveryList) {
	if len(response.Items) < len(api.Items) {
		t.Errorf("expected to check for at least %d groups, only have %d groups in response", len(api.Items), len(response.Items))
	}
	for _, knownGroup := range api.Items {
		found := false
		for _, possibleGroup := range response.Items {
			if knownGroup.Name == possibleGroup.Name {
				t.Logf("found %s", knownGroup.Name)
				found = true
			}
		}
		if found == false {
			t.Errorf("could not find %s", knownGroup.Name)
		}
	}
}

// Test that a handler associated with an APIService gets pinged after the
// APIService has been marked as dirty
func TestDirty(t *testing.T) {
	pinged := false
	service := discoveryendpoint.NewResourceManager()
	aggregatedResourceManager := discoveryendpoint.NewResourceManager()

	aggregatedManager := apiserver.NewDiscoveryManager(aggregatedResourceManager)
	aggregatedManager.AddAPIService(&apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v1.stable.example.com",
		},
		Spec: apiregistrationv1.APIServiceSpec{
			Group:   "stable.example.com",
			Version: "v1",
			Service: &apiregistrationv1.ServiceReference{
				Name: "test-service",
			},
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pinged = true
		service.ServeHTTP(w, r)
	}))
	testCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go aggregatedManager.Run(testCtx.Done())
	cache.WaitForCacheSync(testCtx.Done(), aggregatedManager.ExternalServicesSynced)

	// immediately check for ping, since Run() should block for local services
	if !pinged {
		t.Errorf("service handler never pinged")
	}
}

// Show that an APIService can be removed and that its group no longer remains
// if there are no versions
func TestRemoveAPIService(t *testing.T) {
	aggyService := discoveryendpoint.NewResourceManager()
	service := discoveryendpoint.NewResourceManager()
	apiGroup := fuzzAPIGroups(2, 3, 10)
	service.SetGroups(apiGroup.Items)

	var apiServices []*apiregistrationv1.APIService
	for _, g := range apiGroup.Items {
		for _, v := range g.Versions {
			apiservice := &apiregistrationv1.APIService{
				ObjectMeta: metav1.ObjectMeta{
					Name: v.Version + "." + g.Name,
				},
				Spec: apiregistrationv1.APIServiceSpec{
					Group:   g.Name,
					Version: v.Version,
					Service: &apiregistrationv1.ServiceReference{
						Namespace: "serviceNamespace",
						Name:      "serviceName",
					},
				},
			}

			apiServices = append(apiServices, apiservice)
		}
	}

	aggregatedManager := apiserver.NewDiscoveryManager(aggyService)

	for _, s := range apiServices {
		aggregatedManager.AddAPIService(s, service)
	}

	testCtx, _ := context.WithCancel(context.Background())
	go aggregatedManager.Run(testCtx.Done())

	for _, s := range apiServices {
		aggregatedManager.RemoveAPIService(s.Name)
	}

	cache.WaitForCacheSync(testCtx.Done(), aggregatedManager.ExternalServicesSynced)

	response, _, parsed := fetchPath(aggyService, "")
	if response.StatusCode != 200 {
		t.Fatalf("unexpected status code %d", response.StatusCode)
	}
	if len(parsed.Items) > 0 {
		t.Errorf("expected to find no groups after service deletion (got %d groups)", len(parsed.Items))
	}
}

func TestLegacyFallback(t *testing.T) {
	aggregatedResourceManager := discoveryendpoint.NewResourceManager()

	legacyGroupHandler := discovery.NewAPIGroupHandler(scheme.Codecs, metav1.APIGroup{
		Name: "stable.example.com",
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "stable.example.com/v1",
			Version:      "v1",
		},
		Versions: []metav1.GroupVersionForDiscovery{
			{
				GroupVersion: "stable.example.com/v1",
				Version:      "v1",
			},
			{
				GroupVersion: "stable.example.com/v1beta1",
				Version:      "v1beta1",
			},
		},
	})

	resource := metav1.APIResource{
		Name:         "foos",
		SingularName: "foo",
		Group:        "stable.example.com",
		Version:      "v1",
		Namespaced:   false,
		Kind:         "Foo",
		Verbs:        []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"},
		Categories:   []string{"all"},
	}

	legacyResourceHandler := discovery.NewAPIVersionHandler(scheme.Codecs, schema.GroupVersion{
		Group:   "stable.example.com",
		Version: "v1",
	}, discovery.APIResourceListerFunc(func() []metav1.APIResource {
		return []metav1.APIResource{
			resource,
		}
	}))

	aggregatedManager := apiserver.NewDiscoveryManager(aggregatedResourceManager)
	aggregatedManager.AddAPIService(&apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name: "v1.stable.example.com",
		},
		Spec: apiregistrationv1.APIServiceSpec{
			Group:   "stable.example.com",
			Version: "v1",
			Service: &apiregistrationv1.ServiceReference{
				Name: "test-service",
			},
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/stable.example.com" {
			legacyGroupHandler.ServeHTTP(w, r)
		} else if r.URL.Path == "/apis/stable.example.com/v1" {
			// defer to legacy discovery
			legacyResourceHandler.ServeHTTP(w, r)
		} else {
			// Unknown url
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	testCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go aggregatedManager.Run(testCtx.Done())
	require.True(t, cache.WaitForCacheSync(testCtx.Done(), aggregatedManager.ExternalServicesSynced))

	// At this point external services have synced. Check if discovery document
	// includes the legacy resources
	_, _, doc := fetchPath(aggregatedResourceManager, "")

	converted, err := endpoints.ConvertGroupVersionIntoToDiscovery([]metav1.APIResource{resource})
	require.NoError(t, err)
	require.Equal(t, []apidiscoveryv2beta1.APIGroupDiscovery{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: resource.Group,
			},
			Versions: []apidiscoveryv2beta1.APIVersionDiscovery{
				{
					Version:   resource.Version,
					Resources: converted,
					Freshness: apidiscoveryv2beta1.DiscoveryFreshnessCurrent,
				},
			},
		},
	}, doc.Items)
}

// copied from staging/src/k8s.io/apiserver/pkg/endpoints/discovery/v2/handler_test.go
func fuzzAPIGroups(atLeastNumGroups, maxNumGroups int, seed int64) apidiscoveryv2beta1.APIGroupDiscoveryList {
	fuzzer := fuzz.NewWithSeed(seed)
	fuzzer.NumElements(atLeastNumGroups, maxNumGroups)
	fuzzer.NilChance(0)
	fuzzer.Funcs(func(o *apidiscoveryv2beta1.APIGroupDiscovery, c fuzz.Continue) {
		c.FuzzNoCustom(o)

		// The ResourceManager will just not serve the grouop if its versions
		// list is empty
		atLeastOne := apidiscoveryv2beta1.APIVersionDiscovery{}
		c.Fuzz(&atLeastOne)
		o.Versions = append(o.Versions, atLeastOne)

		o.TypeMeta = metav1.TypeMeta{
			Kind:       "APIGroupDiscovery",
			APIVersion: "v1",
		}
	})

	var apis []apidiscoveryv2beta1.APIGroupDiscovery
	fuzzer.Fuzz(&apis)

	return apidiscoveryv2beta1.APIGroupDiscoveryList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIGroupDiscoveryList",
			APIVersion: "v1",
		},
		Items: apis,
	}

}

// copied from staging/src/k8s.io/apiserver/pkg/endpoints/discovery/v2/handler_test.go
func fetchPath(handler http.Handler, etag string) (*http.Response, []byte, *apidiscoveryv2beta1.APIGroupDiscoveryList) {
	// Expect json-formatted apis group list
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis", nil)

	// Ask for JSON response
	req.Header.Set("Accept", runtime.ContentTypeJSON+";g=apidiscovery.k8s.io;v=v2beta1;as=APIGroupDiscoveryList")

	if etag != "" {
		// Quote provided etag if unquoted
		quoted := etag
		if !strings.HasPrefix(etag, "\"") {
			quoted = strconv.Quote(etag)
		}
		req.Header.Set("If-None-Match", quoted)
	}

	handler.ServeHTTP(w, req)

	bytes := w.Body.Bytes()
	var decoded *apidiscoveryv2beta1.APIGroupDiscoveryList
	if len(bytes) > 0 {
		decoded = &apidiscoveryv2beta1.APIGroupDiscoveryList{}
		runtime.DecodeInto(scheme.Codecs.UniversalDecoder(), bytes, decoded)
	}

	return w.Result(), bytes, decoded
}
