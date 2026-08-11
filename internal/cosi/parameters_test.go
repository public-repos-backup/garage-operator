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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBucketClassParameters(t *testing.T) {
	tests := []struct {
		name        string
		params      map[string]string
		wantErr     bool
		wantCluster string
		wantNS      string
	}{
		{
			name: "valid with namespace",
			params: map[string]string{
				testClusterRef:       testMyCluster,
				testClusterNamespace: testGarageSystem,
			},
			wantCluster: testMyCluster,
			wantNS:      testGarageSystem,
		},
		{
			name: "valid without namespace uses default",
			params: map[string]string{
				testClusterRef: testMyCluster,
			},
			wantCluster: testMyCluster,
			wantNS:      "default",
		},
		{
			name:    "missing clusterRef",
			params:  map[string]string{},
			wantErr: true,
		},
		{
			name: "negative maxSize",
			params: map[string]string{
				testClusterRef: testMyCluster,
				paramMaxSize:   "-1Gi",
			},
			wantErr: true,
		},
		{
			name: "negative maxObjects",
			params: map[string]string{
				testClusterRef:  testMyCluster,
				paramMaxObjects: "-1",
			},
			wantErr: true,
		},
		{
			name: "invalid websiteEnabled",
			params: map[string]string{
				testClusterRef:      testMyCluster,
				paramWebsiteEnabled: "treu",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := ParseBucketClassParameters(tt.params, "default")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCluster, params.ClusterRef)
			assert.Equal(t, tt.wantNS, params.ClusterNamespace)
		})
	}
}

func TestParseBucketAccessClassParameters(t *testing.T) {
	tests := []struct {
		name        string
		params      map[string]string
		wantErr     bool
		wantCluster string
		wantNS      string
	}{
		{
			name: "valid with namespace",
			params: map[string]string{
				testClusterRef:       testMyCluster,
				testClusterNamespace: testGarageSystem,
			},
			wantCluster: testMyCluster,
			wantNS:      testGarageSystem,
		},
		{
			name: "valid without namespace uses default",
			params: map[string]string{
				testClusterRef: testMyCluster,
			},
			wantCluster: testMyCluster,
			wantNS:      "default",
		},
		{
			name:    "missing clusterRef",
			params:  map[string]string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := ParseBucketAccessClassParameters(tt.params, "default")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCluster, params.ClusterRef)
			assert.Equal(t, tt.wantNS, params.ClusterNamespace)
		})
	}
}

func TestParseBucketClassParameters_UnknownParams(t *testing.T) {
	_, err := ParseBucketClassParameters(map[string]string{
		testClusterRef: testMyCluster,
		"unknownKey":   "somevalue",
		"anotherKey":   "anothervalue",
	}, "default")
	require.EqualError(t, err, "unsupported BucketClass parameters: anotherKey, unknownKey")
}

func TestParseBucketAccessClassParameters_UnknownParams(t *testing.T) {
	_, err := ParseBucketAccessClassParameters(map[string]string{
		testClusterRef: testMyCluster,
		"extra":        "ignored",
	}, "default")
	require.EqualError(t, err, "unsupported BucketAccessClass parameters: extra")
}
