// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package overlay_test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"storj.io/storj/internal/testcontext"
	"storj.io/storj/internal/testplanet"
	"storj.io/storj/pkg/overlay"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/statdb"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/satellitedb/satellitedbtest"
)

func TestCache_Database(t *testing.T) {
	t.Parallel()

	satellitedbtest.Run(t, func(t *testing.T, db satellite.DB) {
		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		testCache(ctx, t, db.OverlayCache(), db.StatDB())
	})
}

func testCache(ctx context.Context, t *testing.T, store overlay.DB, sdb statdb.DB) {
	valid1ID := storj.NodeID{}
	valid2ID := storj.NodeID{}
	missingID := storj.NodeID{}

	_, _ = rand.Read(valid1ID[:])
	_, _ = rand.Read(valid2ID[:])
	_, _ = rand.Read(missingID[:])

	cache := overlay.NewCache(store, sdb)

	{ // Put
		err := cache.Put(ctx, valid1ID, pb.Node{Id: valid1ID})
		if err != nil {
			t.Fatal(err)
		}

		err = cache.Put(ctx, valid2ID, pb.Node{Id: valid2ID})
		if err != nil {
			t.Fatal(err)
		}
	}

	{ // Get
		_, err := cache.Get(ctx, storj.NodeID{})
		assert.Error(t, err)
		assert.True(t, err == overlay.ErrEmptyNode)

		valid1, err := cache.Get(ctx, valid1ID)
		if assert.NoError(t, err) {
			assert.Equal(t, valid1.Id, valid1ID)
		}

		valid2, err := cache.Get(ctx, valid2ID)
		if assert.NoError(t, err) {
			assert.Equal(t, valid2.Id, valid2ID)
		}

		invalid2, err := cache.Get(ctx, missingID)
		assert.Error(t, err)
		assert.True(t, err == overlay.ErrNodeNotFound)
		assert.Nil(t, invalid2)

		// TODO: add erroring database test
	}

	{ // GetAll
		nodes, err := cache.GetAll(ctx, storj.NodeIDList{valid2ID, valid1ID, valid2ID})
		assert.NoError(t, err)
		assert.Equal(t, nodes[0].Id, valid2ID)
		assert.Equal(t, nodes[1].Id, valid1ID)
		assert.Equal(t, nodes[2].Id, valid2ID)

		nodes, err = cache.GetAll(ctx, storj.NodeIDList{valid1ID, missingID})
		assert.NoError(t, err)
		assert.Equal(t, nodes[0].Id, valid1ID)
		assert.Nil(t, nodes[1])

		nodes, err = cache.GetAll(ctx, make(storj.NodeIDList, 2))
		assert.NoError(t, err)
		assert.Nil(t, nodes[0])
		assert.Nil(t, nodes[1])

		_, err = cache.GetAll(ctx, storj.NodeIDList{})
		assert.True(t, overlay.OverlayError.Has(err))

		// TODO: add erroring database test
	}

	{ // List
		list, err := cache.List(ctx, storj.NodeID{}, 3)
		assert.NoError(t, err)
		assert.NotNil(t, list)
	}

	{ // Paginate

		// should return two nodes
		nodes, more, err := cache.Paginate(ctx, 0, 2)
		assert.NotNil(t, more)
		assert.NoError(t, err)
		assert.Equal(t, len(nodes), 2)

		// should return no nodes
		zero, more, err := cache.Paginate(ctx, 0, 0)
		assert.NoError(t, err)
		assert.NotNil(t, more)
		assert.NotEqual(t, len(zero), 0)
	}

	{ // Delete
		// Test standard delete
		err := cache.Delete(ctx, valid1ID)
		assert.NoError(t, err)

		// Check that it was deleted
		deleted, err := cache.Get(ctx, valid1ID)
		assert.Error(t, err)
		assert.Nil(t, deleted)
		assert.True(t, err == overlay.ErrNodeNotFound)

		// Test idempotent delete / non existent key delete
		err = cache.Delete(ctx, valid1ID)
		assert.NoError(t, err)

		// Test empty key delete
		err = cache.Delete(ctx, storj.NodeID{})
		assert.Error(t, err)
		assert.True(t, err == overlay.ErrEmptyNode)
	}
}

func TestRandomizedSelection(t *testing.T) {
	t.Parallel()

	totalNodes := 10
	selectIterations := 100
	numNodesToSelect := 1
	minSelectCount := (selectIterations * numNodesToSelect / totalNodes) / 2

	testplanet.Run(t, testplanet.Config{
		SatelliteCount: 1, StorageNodeCount: totalNodes, UplinkCount: 0,
	}, func(t *testing.T, ctx *testcontext.Context, planet *testplanet.Planet) {
		// we wait for all the nodes to complete bootstrapping off the satellite
		time.Sleep(10 * time.Second)

		nodeCounts := make(map[storj.NodeID]int)

		cache := planet.Satellites[0].DB.OverlayCache()
		// select numNodesToSelect nodes selectIterations times
		for i := 0; i < selectIterations; i++ {
			nodes, err := cache.SelectNodes(ctx, numNodesToSelect, &overlay.NodeCriteria{
				FreeBandwidth:      0,
				FreeDisk:           0,
				AuditCount:         0,
				AuditSuccessRatio:  0,
				UptimeCount:        0,
				UptimeSuccessRatio: 0,
			})
			require.NoError(t, err)
			require.Len(t, nodes, numNodesToSelect)

			for _, node := range nodes {
				nodeCounts[node.Id]++
			}
		}

		// expect that each node has been selected at least minSelectCount times
		for _, node := range planet.StorageNodes {
			count := nodeCounts[node.ID()]
			assert.True(t, count >= minSelectCount)
		}
	})
}
