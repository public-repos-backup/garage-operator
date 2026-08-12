/*
Copyright 2026 Raj Singh.

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

package cosi

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Parameter key constants
const (
	paramClusterRef       = "clusterRef"
	paramClusterNamespace = "clusterNamespace"
	paramMaxSize          = "maxSize"
	paramMaxObjects       = "maxObjects"
	paramWebsiteEnabled   = "websiteEnabled"
	paramTrue             = "true"
)

// BucketClassParameters holds parsed BucketClass parameters
type BucketClassParameters struct {
	ClusterRef       string
	ClusterNamespace string
	MaxSize          *resource.Quantity
	MaxObjects       *int64
	WebsiteEnabled   bool
}

// BucketAccessClassParameters holds parsed BucketAccessClass parameters
type BucketAccessClassParameters struct {
	ClusterRef       string
	ClusterNamespace string
}

var knownBucketClassParams = map[string]struct{}{
	paramClusterRef: {}, paramClusterNamespace: {}, paramMaxSize: {}, paramMaxObjects: {}, paramWebsiteEnabled: {},
}

var knownBucketAccessClassParams = map[string]struct{}{
	paramClusterRef: {}, paramClusterNamespace: {},
}

// ParseBucketClassParameters parses BucketClass parameters
func ParseBucketClassParameters(params map[string]string, defaultNamespace string) (*BucketClassParameters, error) {
	if unknown := unknownParameters(params, knownBucketClassParams); len(unknown) > 0 {
		return nil, fmt.Errorf("unsupported BucketClass parameters: %s", strings.Join(unknown, ", "))
	}
	clusterRef, ok := params[paramClusterRef]
	if !ok || clusterRef == "" {
		return nil, fmt.Errorf("required parameter 'clusterRef' not specified")
	}

	clusterNS := params[paramClusterNamespace]
	if clusterNS == "" {
		clusterNS = defaultNamespace
	}

	p := &BucketClassParameters{
		ClusterRef:       clusterRef,
		ClusterNamespace: clusterNS,
	}

	if maxSize, ok := params[paramMaxSize]; ok {
		q, err := resource.ParseQuantity(maxSize)
		if err != nil {
			return nil, fmt.Errorf("invalid maxSize: %w", err)
		}
		if q.Sign() < 0 {
			return nil, fmt.Errorf("invalid maxSize: must not be negative")
		}
		p.MaxSize = &q
	}

	if maxObjects, ok := params[paramMaxObjects]; ok {
		n, err := strconv.ParseInt(maxObjects, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid maxObjects: %w", err)
		}
		if n < 0 {
			return nil, fmt.Errorf("invalid maxObjects: must not be negative")
		}
		p.MaxObjects = &n
	}

	if websiteEnabled, ok := params[paramWebsiteEnabled]; ok {
		switch websiteEnabled {
		case paramTrue:
			p.WebsiteEnabled = true
		case "false":
			p.WebsiteEnabled = false
		default:
			return nil, fmt.Errorf("invalid websiteEnabled %q: must be true or false", websiteEnabled)
		}
	}

	return p, nil
}

// ParseBucketAccessClassParameters parses BucketAccessClass parameters
func ParseBucketAccessClassParameters(params map[string]string, defaultNamespace string) (*BucketAccessClassParameters, error) {
	if unknown := unknownParameters(params, knownBucketAccessClassParams); len(unknown) > 0 {
		return nil, fmt.Errorf("unsupported BucketAccessClass parameters: %s", strings.Join(unknown, ", "))
	}
	clusterRef, ok := params[paramClusterRef]
	if !ok || clusterRef == "" {
		return nil, fmt.Errorf("required parameter 'clusterRef' not specified")
	}

	clusterNS := params[paramClusterNamespace]
	if clusterNS == "" {
		clusterNS = defaultNamespace
	}

	p := &BucketAccessClassParameters{
		ClusterRef:       clusterRef,
		ClusterNamespace: clusterNS,
	}

	return p, nil
}

func unknownParameters(params map[string]string, known map[string]struct{}) []string {
	unknown := make([]string, 0)
	for key := range params {
		if _, ok := known[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}
