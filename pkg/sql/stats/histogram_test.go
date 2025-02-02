// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package stats

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
)

func TestEquiDepthHistogram(t *testing.T) {
	type expBucket struct {
		upper   int
		numEq   int64
		numLess int64
	}
	testCases := []struct {
		samples    []int
		numRows    int64
		maxBuckets int
		buckets    []expBucket
	}{
		{
			samples:    []int{1, 2, 4, 5, 5, 9},
			numRows:    6,
			maxBuckets: 2,
			buckets: []expBucket{
				{
					// Bucket contains 1, 2, 4.
					upper: 4, numEq: 1, numLess: 2,
				},
				{
					// Bucket contains 5, 5, 9.
					upper: 9, numEq: 1, numLess: 2,
				},
			},
		},
		{
			samples:    []int{1, 1, 1, 1, 2, 2},
			numRows:    6,
			maxBuckets: 2,
			buckets: []expBucket{
				{
					// Bucket contains 1, 1, 1, 1.
					upper: 1, numEq: 4, numLess: 0,
				},
				{
					// Bucket contains 2, 2.
					upper: 2, numEq: 2, numLess: 0,
				},
			},
		},
		{
			samples:    []int{1, 1, 1, 1, 2, 2},
			numRows:    6,
			maxBuckets: 3,
			buckets: []expBucket{
				{
					// Bucket contains 1, 1, 1, 1.
					upper: 1, numEq: 4, numLess: 0,
				},
				{
					// Bucket contains 2, 2.
					upper: 2, numEq: 2, numLess: 0,
				},
			},
		},
		{
			samples:    []int{1, 1, 2, 2, 2, 2},
			numRows:    6,
			maxBuckets: 2,
			buckets: []expBucket{
				{
					// Bucket contains everything.
					upper: 2, numEq: 4, numLess: 2,
				},
			},
		},
		{
			samples:    []int{1, 1, 2, 2, 2, 2},
			numRows:    6,
			maxBuckets: 3,
			buckets: []expBucket{
				{
					// Bucket contains 1, 1.
					upper: 1, numEq: 2, numLess: 0,
				},
				{
					// Bucket contains 2, 2, 2, 2.
					upper: 2, numEq: 4, numLess: 0,
				},
			},
		},
		{
			samples:    []int{1, 1, 1, 1, 1, 1},
			numRows:    600,
			maxBuckets: 10,
			buckets: []expBucket{
				{
					// Bucket contains everything.
					upper: 1, numEq: 600, numLess: 0,
				},
			},
		},
		{
			samples:    []int{1, 2, 3, 4},
			numRows:    4000,
			maxBuckets: 3,
			buckets: []expBucket{
				{
					// Bucket contains 1.
					upper: 1, numEq: 1000, numLess: 0,
				},
				{
					// Bucket contains 2.
					upper: 2, numEq: 1000, numLess: 0,
				},
				{
					// Bucket contains 3, 4.
					upper: 4, numEq: 1000, numLess: 1000,
				},
			},
		},
	}

	evalCtx := tree.NewTestingEvalContext(cluster.MakeTestingClusterSettings())

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			samples := make(tree.Datums, len(tc.samples))
			perm := rand.Perm(len(samples))
			for i := range samples {
				// Randomly permute the samples.
				val := tc.samples[perm[i]]

				samples[i] = tree.NewDInt(tree.DInt(val))
			}

			h, err := EquiDepthHistogram(evalCtx, samples, tc.numRows, tc.maxBuckets)
			if err != nil {
				t.Fatal(err)
			}
			if len(h.Buckets) != len(tc.buckets) {
				t.Fatalf("Invalid number of buckets %d, expected %d", len(h.Buckets), len(tc.buckets))
			}
			for i, b := range h.Buckets {
				_, val, err := encoding.DecodeVarintAscending(b.UpperBound)
				if err != nil {
					t.Fatal(err)
				}
				exp := tc.buckets[i]
				if val != int64(exp.upper) {
					t.Errorf("bucket %d: incorrect boundary %d, expected %d", i, val, exp.upper)
				}
				if b.NumEq != exp.numEq {
					t.Errorf("bucket %d: incorrect EqRows %d, expected %d", i, b.NumEq, exp.numEq)
				}
				if b.NumRange != exp.numLess {
					t.Errorf("bucket %d: incorrect RangeRows %d, expected %d", i, b.NumRange, exp.numLess)
				}
			}
		})
	}

	t.Run("invalid-numRows", func(t *testing.T) {
		samples := tree.Datums{tree.NewDInt(1), tree.NewDInt(2), tree.NewDInt(3)}
		_, err := EquiDepthHistogram(evalCtx, samples, 2 /* numRows */, 10 /* maxBuckets */)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("nulls", func(t *testing.T) {
		samples := tree.Datums{tree.NewDInt(1), tree.NewDInt(2), tree.DNull}
		_, err := EquiDepthHistogram(evalCtx, samples, 100 /* numRows */, 10 /* maxBuckets */)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
