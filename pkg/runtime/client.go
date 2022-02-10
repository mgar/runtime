// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package runtime

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ClientWithoutCache struct {
	reader client.Reader
	client.Client
}

// NewClientWithoutCache returns a new client that is configured to retrieve objects 
// using the API server instead of the cache
func NewClientWithoutCache(c client.Client, r client.Reader) client.Client {
	return &ClientWithoutCache{Client: c, reader: r}
}

func (c *ClientWithoutCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	return c.reader.Get(ctx, key, obj)
}

func (c *ClientWithoutCache) List(ctx context.Context, key client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, key, opts...)
}
