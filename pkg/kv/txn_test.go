// Copyright 2014 The Cockroach Authors.
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

package kv

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/tscache"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/localtestcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/pkg/errors"
)

// TestTxnDBBasics verifies that a simple transaction can be run and
// either committed or aborted. On commit, mutations are visible; on
// abort, mutations are never visible. During the txn, verify that
// uncommitted writes cannot be read outside of the txn but can be
// read from inside the txn.
func TestTxnDBBasics(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDB(t)
	defer s.Stop()
	value := []byte("value")

	for _, commit := range []bool{true, false} {
		key := []byte(fmt.Sprintf("key-%t", commit))

		err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			// Put transactional value.
			if err := txn.Put(ctx, key, value); err != nil {
				return err
			}

			// Attempt to read in another txn.
			conflictTxn := client.NewTxn(ctx, s.DB, 0 /* gatewayNodeID */, client.RootTxn)
			conflictTxn.InternalSetPriority(enginepb.MaxTxnPriority)
			if gr, err := conflictTxn.Get(ctx, key); err != nil {
				return err
			} else if gr.Exists() {
				return errors.Errorf("expected nil value; got %v", gr.Value)
			}

			// Read within the transaction.
			if gr, err := txn.Get(ctx, key); err != nil {
				return err
			} else if !gr.Exists() || !bytes.Equal(gr.ValueBytes(), value) {
				return errors.Errorf("expected value %q; got %q", value, gr.Value)
			}

			if !commit {
				return errors.Errorf("purposefully failing transaction")
			}
			return nil
		})

		if commit != (err == nil) {
			t.Errorf("expected success? %t; got %s", commit, err)
		} else if !commit && !testutils.IsError(err, "purposefully failing transaction") {
			t.Errorf("unexpected failure with !commit: %v", err)
		}

		// Verify the value is now visible on commit == true, and not visible otherwise.
		gr, err := s.DB.Get(context.TODO(), key)
		if commit {
			if err != nil || !gr.Exists() || !bytes.Equal(gr.ValueBytes(), value) {
				t.Errorf("expected success reading value: %+v, %s", gr.ValueBytes(), err)
			}
		} else {
			if err != nil || gr.Exists() {
				t.Errorf("expected success and nil value: %s, %s", gr, err)
			}
		}
	}
}

// BenchmarkSingleRoundtripWithLatency runs a number of transactions writing
// to the same key back to back in a single round-trip. Latency is simulated
// by pausing before each RPC sent.
func BenchmarkSingleRoundtripWithLatency(b *testing.B) {
	for _, latency := range []time.Duration{0, 10 * time.Millisecond} {
		b.Run(fmt.Sprintf("latency=%s", latency), func(b *testing.B) {
			var s localtestcluster.LocalTestCluster
			s.Latency = latency
			s.Start(b, testutils.NewNodeTestBaseContext(), InitFactoryForLocalTestCluster)
			defer s.Stop()
			defer b.StopTimer()
			key := roachpb.Key("key")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
					b := txn.NewBatch()
					b.Put(key, fmt.Sprintf("value-%d", i))
					return txn.CommitInBatch(ctx, b)
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestLostUpdate verifies that transactions are not susceptible to the
// lost update anomaly.
//
// The transaction history looks as follows ("2" refers to the
// independent goroutine's actions)
//
//   R1(A) W2(A,"hi") W1(A,"oops!") C1 [serializable restart] R1(A) W1(A,"correct") C1
func TestLostUpdate(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDB(t)
	defer s.Stop()
	var key = roachpb.Key("a")

	done := make(chan error)
	start := make(chan struct{})
	go func() {
		<-start
		done <- s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			return txn.Put(ctx, key, "hi")
		})
	}()

	if err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
		// Issue a read to get initial value.
		gr, err := txn.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if txn.Epoch() == 0 {
			close(start) // let someone write into our future
			// When they're done, write based on what we read.
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		} else if txn.Epoch() > 1 {
			t.Fatal("should experience just one restart")
		}

		if gr.Exists() && bytes.Equal(gr.ValueBytes(), []byte("hi")) {
			if err := txn.Put(ctx, key, "correct"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := txn.Put(ctx, key, "oops!"); err != nil {
				t.Fatal(err)
			}
		}
		// Verify that the WriteTooOld boolean is set on the txn.
		proto := txn.Serialize()
		if (txn.Epoch() == 0) != proto.WriteTooOld {
			t.Fatalf("expected write too old set (%t): got %t", (txn.Epoch() == 0), proto.WriteTooOld)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Verify final value.
	gr, err := s.DB.Get(context.TODO(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !gr.Exists() || !bytes.Equal(gr.ValueBytes(), []byte("correct")) {
		t.Fatalf("expected \"correct\", got %q", gr.ValueBytes())
	}
}

// TestPriorityRatchetOnAbortOrPush verifies that the priority of
// a transaction is ratcheted by successive aborts or pushes. In
// particular, we want to ensure ratcheted priorities when the txn
// discovers it's been aborted or pushed through a poisoned sequence
// cache. This happens when a concurrent writer aborts an intent or a
// concurrent reader pushes an intent.
func TestPriorityRatchetOnAbortOrPush(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDB(t)
	defer s.Stop()

	pushByReading := func(key roachpb.Key) {
		if err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			if err := txn.SetUserPriority(roachpb.MaxUserPriority); err != nil {
				t.Fatal(err)
			}
			_, err := txn.Get(ctx, key)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	abortByWriting := func(key roachpb.Key) {
		if err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			if err := txn.SetUserPriority(roachpb.MaxUserPriority); err != nil {
				t.Fatal(err)
			}
			return txn.Put(ctx, key, "foo")
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Try both read and write.
	for _, read := range []bool{true, false} {
		var iteration int
		if err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			defer func() { iteration++ }()
			key := roachpb.Key(fmt.Sprintf("read=%t", read))

			// Write to lay down an intent (this will send the begin
			// transaction which gets the updated priority).
			if err := txn.Put(ctx, key, "bar"); err != nil {
				return err
			}

			if iteration == 1 {
				// Verify our priority has ratcheted to one less than the pusher's priority
				expPri := enginepb.MaxTxnPriority - 1
				if pri := txn.Serialize().Priority; pri != expPri {
					t.Fatalf("%s: expected priority on retry to ratchet to %d; got %d", key, expPri, pri)
				}
				return nil
			}

			// Now simulate a concurrent reader or writer. Our txn will
			// either be pushed or aborted. Then issue a read and verify
			// that if we've been pushed, no error is returned and if we
			// have been aborted, we get an aborted error.
			var err error
			if read {
				pushByReading(key)
				_, err = txn.Get(ctx, key)
				if err != nil {
					t.Fatalf("%s: expected no error; got %s", key, err)
				}
			} else {
				abortByWriting(key)
				_, err = txn.Get(ctx, key)
				assertTransactionAbortedError(t, err)
			}

			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTxnTimestampRegression verifies that if a transaction's
// timestamp is pushed forward by a concurrent read, it may still
// commit. A bug in the EndTransaction implementation used to compare
// the transaction's current timestamp instead of original timestamp.
func TestTxnTimestampRegression(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDB(t)
	defer s.Stop()

	keyA := "a"
	keyB := "b"
	err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
		// Put transactional value.
		if err := txn.Put(ctx, keyA, "value1"); err != nil {
			return err
		}

		// Attempt to read in another txn (this will push timestamp of transaction).
		conflictTxn := client.NewTxn(ctx, s.DB, 0 /* gatewayNodeID */, client.RootTxn)
		conflictTxn.InternalSetPriority(enginepb.MaxTxnPriority)
		if _, err := conflictTxn.Get(context.TODO(), keyA); err != nil {
			return err
		}

		// Now, read again outside of txn to warmup timestamp cache with higher timestamp.
		if _, err := s.DB.Get(context.TODO(), keyB); err != nil {
			return err
		}

		// Write now to keyB, which will get a higher timestamp than keyB was written at.
		return txn.Put(ctx, keyB, "value2")
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTxnLongDelayBetweenWritesWithConcurrentRead simulates a
// situation where the delay between two writes in a txn is longer
// than 10 seconds.
// See issue #676 for full details about original bug.
func TestTxnLongDelayBetweenWritesWithConcurrentRead(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDB(t)
	defer s.Stop()

	keyA := roachpb.Key("a")
	keyB := roachpb.Key("b")
	ch := make(chan struct{})
	errChan := make(chan error)
	go func() {
		errChan <- s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			// Put transactional value.
			if err := txn.Put(ctx, keyA, "value1"); err != nil {
				return err
			}
			// Notify txnB do 1st get(b).
			ch <- struct{}{}
			// Wait for txnB notify us to put(b).
			<-ch
			// Write now to keyB.
			return txn.Put(ctx, keyB, "value2")
		})
	}()

	// Wait till txnA finish put(a).
	<-ch
	// Delay for longer than the cache window.
	s.Manual.Increment((tscache.MinRetentionWindow + time.Second).Nanoseconds())
	if err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
		// Attempt to get first keyB.
		gr1, err := txn.Get(ctx, keyB)
		if err != nil {
			return err
		}
		// Notify txnA put(b).
		ch <- struct{}{}
		// Wait for txnA finish commit.
		if err := <-errChan; err != nil {
			t.Fatal(err)
		}
		// get(b) again.
		gr2, err := txn.Get(ctx, keyB)
		if err != nil {
			return err
		}

		if gr1.Exists() || gr2.Exists() {
			t.Fatalf("Repeat read same key in same txn but get different value gr1: %q, gr2 %q", gr1.Value, gr2.Value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTxnRepeatGetWithRangeSplit simulates two writes in a single
// transaction, with a range split occurring between. The second write
// is sent to the new range. The test verifies that another transaction
// reading before and after the split will read the same values.
// See issue #676 for full details about original bug.
func TestTxnRepeatGetWithRangeSplit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDBWithContextAndKnobs(t, client.DefaultDBContext(), &storage.StoreTestingKnobs{
		DisableScanner:    true,
		DisableSplitQueue: true,
		DisableMergeQueue: true,
	})
	defer s.Stop()

	keyA := roachpb.Key("a")
	keyC := roachpb.Key("c")
	splitKey := roachpb.Key("b")
	ch := make(chan struct{})
	errChan := make(chan error)
	go func() {
		errChan <- s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			// Put transactional value.
			if err := txn.Put(ctx, keyA, "value1"); err != nil {
				return err
			}
			// Notify txnB do 1st get(c).
			ch <- struct{}{}
			// Wait for txnB notify us to put(c).
			<-ch
			// Write now to keyC, which will keep timestamp.
			return txn.Put(ctx, keyC, "value2")
		})
	}()

	// Wait till txnA finish put(a).
	<-ch

	if err := s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
		// First get keyC, value will be nil.
		gr1, err := txn.Get(ctx, keyC)
		if err != nil {
			return err
		}
		s.Manual.Increment(time.Second.Nanoseconds())
		// Split range by keyB.
		if err := s.DB.AdminSplit(context.TODO(), splitKey, splitKey, true /* manual */); err != nil {
			t.Fatal(err)
		}
		// Wait till split complete.
		// Check that we split 1 times in allotted time.
		testutils.SucceedsSoon(t, func() error {
			// Scan the meta records.
			rows, serr := s.DB.Scan(context.TODO(), keys.Meta2Prefix, keys.MetaMax, 0)
			if serr != nil {
				t.Fatalf("failed to scan meta2 keys: %s", serr)
			}
			if len(rows) >= 2 {
				return nil
			}
			return errors.Errorf("failed to split")
		})
		// Notify txnA put(c).
		ch <- struct{}{}
		// Wait for txnA finish commit.
		if err := <-errChan; err != nil {
			t.Fatal(err)
		}
		// Get(c) again.
		gr2, err := txn.Get(ctx, keyC)
		if err != nil {
			return err
		}

		if !gr1.Exists() && gr2.Exists() {
			t.Fatalf("Repeat read same key in same txn but get different value gr1 nil gr2 %v", gr2.Value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTxnRestartedSerializableTimestampRegression verifies that there is
// no timestamp regression error in the event that a pushed txn record disagrees
// with the original timestamp of a restarted transaction.
func TestTxnRestartedSerializableTimestampRegression(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDB(t)
	defer s.Stop()

	keyA := "a"
	keyB := "b"
	ch := make(chan struct{})
	errChan := make(chan error)
	var count int
	go func() {
		errChan <- s.DB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			count++
			// Use a low priority for the transaction so that it can be pushed.
			if err := txn.SetUserPriority(roachpb.MinUserPriority); err != nil {
				t.Fatal(err)
			}

			// Put transactional value.
			if err := txn.Put(ctx, keyA, "value1"); err != nil {
				return err
			}
			if count <= 1 {
				// Notify concurrent getter to push txnA on get(a).
				ch <- struct{}{}
				// Wait for txnB notify us to commit.
				<-ch
			}
			// Do a write to keyB, which will forward txn timestamp.
			return txn.Put(ctx, keyB, "value2")
		})
	}()

	// Wait until txnA finishes put(a).
	<-ch
	// Attempt to get keyA, which will push txnA.
	if _, err := s.DB.Get(context.TODO(), keyA); err != nil {
		t.Fatal(err)
	}
	// Do a read at keyB to cause txnA to forward timestamp.
	if _, err := s.DB.Get(context.TODO(), keyB); err != nil {
		t.Fatal(err)
	}
	// Notify txnA to commit.
	ch <- struct{}{}

	// Wait for txnA to finish.
	if err := <-errChan; err != nil {
		t.Fatal(err)
	}
	// We expect no restarts (so a count of one). The transaction continues
	// despite the push and timestamp forwarding in order to lay down all
	// intents in the first pass. On the first EndTransaction, the difference
	// in timestamps would cause the serializable transaction to update spans,
	// but only writes occurred during the transaction, so the commit succeeds.
	const expCount = 1
	if count != expCount {
		t.Fatalf("expected %d restarts, but got %d", expCount, count)
	}
}

// TestTxnResolveIntentsFromMultipleEpochs verifies that that intents
// from earlier epochs are cleaned up on transaction commit.
func TestTxnResolveIntentsFromMultipleEpochs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s := createTestDB(t)
	defer s.Stop()

	writeSkewKey := "write-skew"
	keys := []string{"a", "b", "c"}
	ch := make(chan struct{})
	errChan := make(chan error, 1)
	// Launch goroutine to write the three keys on three successive epochs.
	go func() {
		var count int
		errChan <- s.DB.Txn(context.Background(), func(ctx context.Context, txn *client.Txn) error {
			// Read the write skew key, which will be written by another goroutine
			// to ensure transaction restarts.
			if _, err := txn.Get(ctx, writeSkewKey); err != nil {
				return err
			}
			// Signal that the transaction has (re)started.
			ch <- struct{}{}
			// Wait for concurrent writer to write key.
			<-ch
			// Now write our version over the top (will get a pushed timestamp).
			if err := txn.Put(ctx, keys[count], "txn"); err != nil {
				return err
			}
			count++
			return nil
		})
	}()

	step := func(key string, causeWriteSkew bool) {
		// Wait for transaction to start.
		<-ch
		if causeWriteSkew {
			// Write to the write skew key to ensure a restart.
			if err := s.DB.Put(context.Background(), writeSkewKey, "skew-"+key); err != nil {
				t.Fatal(err)
			}
		}
		// Read key to push txn's timestamp forward on its write.
		if _, err := s.DB.Get(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		// Signal the transaction to continue.
		ch <- struct{}{}
	}

	// Step 1 causes a restart.
	step(keys[0], true)
	// Step 2 causes a restart.
	step(keys[1], true)
	// Step 3 does not result in a restart.
	step(keys[2], false)

	// Wait for txn to finish.
	if err := <-errChan; err != nil {
		t.Fatal(err)
	}

	// Read values for three keys. The first two should be empty, the last should be "txn".
	for i, k := range keys {
		v, err := s.DB.Get(context.TODO(), k)
		if err != nil {
			t.Fatal(err)
		}
		str := string(v.ValueBytes())
		if i < len(keys)-1 {
			if str != "" {
				t.Errorf("key %s expected \"\"; got %s", k, str)
			}
		} else {
			if str != "txn" {
				t.Errorf("key %s expected \"txn\"; got %s", k, str)
			}
		}
	}
}
