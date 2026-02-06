//go:build linux
// +build linux

package util

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("RunReconcileLoop", func() {
	ginkgo.It("calls doReconcile periodically", func() {
		stopCh := make(chan struct{})
		defer close(stopCh)

		var callCount atomic.Int32
		doReconcile := func() error {
			callCount.Add(1)
			return nil
		}

		reconcilePeriod := 50 * time.Millisecond
		_ = RunReconcileLoop("test", stopCh, nil, reconcilePeriod, doReconcile)

		// Wait for at least 2 reconcile cycles
		gomega.Eventually(func() int32 {
			return callCount.Load()
		}).WithTimeout(time.Second).WithPolling(10 * time.Millisecond).Should(gomega.BeNumerically(">=", 2))
	})

	ginkgo.It("stops on stopCh close", func() {
		stopCh := make(chan struct{})
		var wg sync.WaitGroup

		var callCount atomic.Int32
		doReconcile := func() error {
			callCount.Add(1)
			return nil
		}

		wg.Add(1)
		_ = RunReconcileLoop("test", stopCh, &wg, 50*time.Millisecond, doReconcile)

		// Wait for at least one call
		gomega.Eventually(func() int32 {
			return callCount.Load()
		}).WithTimeout(time.Second).WithPolling(10 * time.Millisecond).Should(gomega.BeNumerically(">=", 1))

		// Stop and wait
		close(stopCh)
		wg.Wait()

		// Record count after stop
		countAfterStop := callCount.Load()

		// Verify no more calls happen
		gomega.Consistently(func() int32 {
			return callCount.Load()
		}).WithTimeout(100 * time.Millisecond).WithPolling(10 * time.Millisecond).Should(gomega.Equal(countAfterStop))
	})

	ginkgo.It("triggers immediate reconcile on channel send", func() {
		stopCh := make(chan struct{})
		defer close(stopCh)

		var callCount atomic.Int32
		doReconcile := func() error {
			callCount.Add(1)
			return nil
		}

		// Use a long period so periodic reconcile doesn't interfere
		reconcileCh := RunReconcileLoop("test", stopCh, nil, 10*time.Second, doReconcile)

		// Wait for goroutine to start
		gomega.Eventually(func() bool {
			return true
		}).WithTimeout(50 * time.Millisecond).Should(gomega.BeTrue())

		initialCount := callCount.Load()

		// Trigger immediate reconcile
		reconcileCh <- struct{}{}

		// Wait for reconcile to complete
		gomega.Eventually(func() int32 {
			return callCount.Load()
		}).WithTimeout(time.Second).WithPolling(10 * time.Millisecond).Should(gomega.BeNumerically(">", initialCount))
	})

	ginkgo.It("retries on error", func() {
		stopCh := make(chan struct{})
		defer close(stopCh)

		var callCount atomic.Int32
		doReconcile := func() error {
			count := callCount.Add(1)
			if count < 3 {
				return errors.New("temporary error")
			}
			return nil
		}

		reconcileCh := RunReconcileLoop("test", stopCh, nil, 10*time.Second, doReconcile)

		// Trigger reconcile
		reconcileCh <- struct{}{}

		// Should have been called multiple times due to retries
		gomega.Eventually(func() int32 {
			return callCount.Load()
		}).WithTimeout(time.Second).WithPolling(10 * time.Millisecond).Should(gomega.BeNumerically(">=", 3))
	})
})
