// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package elasticsearch

import (
	"context"
	"fmt"

	"code.gitea.io/gitea/modules/indexer/internal"

	elasticsearch8 "github.com/elastic/go-elasticsearch/v8"
	types8 "github.com/elastic/go-elasticsearch/v8/typedapi/types"
)

var _ internal.Indexer = &IndexerV8{}

// Indexer represents a basic elasticsearch indexer implementation
type IndexerV8 struct {
	Client *elasticsearch8.TypedClient

	url       string
	indexName string
	version   int
	mapping   *types8.TypeMapping
}

func NewIndexerV8(url, indexName string, version int, mapping *types8.TypeMapping) *IndexerV8 {
	return &IndexerV8{
		url:       url,
		indexName: indexName,
		version:   version,
		mapping:   mapping,
	}
}

// Init initializes the indexer
func (i *IndexerV8) Init(ctx context.Context) (bool, error) {
	if i == nil {
		return false, fmt.Errorf("cannot init nil indexer")
	}
	if i.Client != nil {
		return false, fmt.Errorf("indexer is already initialized")
	}

	client, err := i.initClient()
	if err != nil {
		return false, err
	}
	i.Client = client

	exists, err := i.Client.Indices.Exists(i.VersionedIndexName()).Do(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}

	if err := i.createIndex(ctx); err != nil {
		return false, err
	}

	return exists, nil
}

// Ping checks if the indexer is available
func (i *IndexerV8) Ping(ctx context.Context) error {
	if i == nil {
		return fmt.Errorf("cannot ping nil indexer")
	}
	if i.Client == nil {
		return fmt.Errorf("indexer is not initialized")
	}

	resp, err := i.Client.Cluster.Health().Do(ctx)
	if err != nil {
		return err
	}
	if resp.Status.Name != "green" && resp.Status.Name != "yellow" {
		// It's healthy if the status is green, and it's available if the status is yellow,
		// see https://www.elastic.co/guide/en/elasticsearch/reference/current/cluster-health.html
		return fmt.Errorf("status of elasticsearch cluster is %s", resp.Status)
	}
	return nil
}

// Close closes the indexer
func (i *IndexerV8) Close() {
	if i == nil {
		return
	}
	i.Client = nil
}