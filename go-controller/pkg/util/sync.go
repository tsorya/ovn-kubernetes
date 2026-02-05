package util

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func GetChildStopChanWithTimeout(parentStopChan <-chan struct{}, duration time.Duration) chan struct{} {
	childStopChan := make(chan struct{})
	timer := time.NewTicker(duration)
	go func() {
		defer timer.Stop()
		select {
		case <-parentStopChan:
			close(childStopChan)
			return
		case <-childStopChan:
			return
		case <-timer.C:
			close(childStopChan)
			return
		}
	}()
	return childStopChan
}

// WaitForInformerCacheSyncWithTimeout waits for the provided informer caches to be populated with all existing objects
// by their respective informer. This corresponds to a LIST operation on the corresponding resource types.
// WaitForInformerCacheSyncWithTimeout times out and returns false if the provided caches haven't all synchronized within types.InformerSyncTimeout
func WaitForInformerCacheSyncWithTimeout(controllerName string, stopCh <-chan struct{}, cacheSyncs ...cache.InformerSynced) bool {
	return cache.WaitForNamedCacheSync(controllerName, GetChildStopChanWithTimeout(stopCh, types.InformerSyncTimeout), cacheSyncs...)
}

// WaitForHandlerSyncWithTimeout waits for the provided handlers to do a sync on all existing objects for the resource types they're
// watching. This corresponds to adding all existing objects. If that doesn't happen before the provided timeout,
// WaitForInformerCacheSyncWithTimeout times out and returns false.
func WaitForHandlerSyncWithTimeout(controllerName string, stopCh <-chan struct{}, timeout time.Duration, handlerSyncs ...cache.InformerSynced) bool {
	return cache.WaitForNamedCacheSync(controllerName, GetChildStopChanWithTimeout(stopCh, timeout), handlerSyncs...)
}

// RunReconcileLoop starts a periodic reconciliation loop with retry logic.
// It returns a channel that can be used to trigger immediate reconciliation.
// Parameters:
//   - name: identifier for logging
//   - stopChan: channel to stop the loop
//   - wg: optional wait group (can be nil)
//   - period: reconciliation period
//   - doReconcile: function to call for reconciliation (must return error for retry logic)
func RunReconcileLoop(name string, stopChan <-chan struct{}, wg *sync.WaitGroup, period time.Duration, doReconcile func() error) chan struct{} {
	reconcileCh := make(chan struct{}, 1)
	triggerReconcile := func() { reconcileCh <- struct{}{} }

	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		timer := time.NewTicker(period)
		defer timer.Stop()
		klog.Infof("Starting %s reconciliation loop (period: %v)", name, period)
		for {
			select {
			case <-stopChan:
				klog.Infof("Stopping %s reconciliation loop", name)
				return
			case <-timer.C:
				triggerReconcile()
			case <-reconcileCh:
				err := retry.OnError(
					wait.Backoff{
						Duration: 10 * time.Millisecond,
						Steps:    4,
						Factor:   5.0,
						Cap:      period,
					},
					func(error) bool {
						select {
						case <-stopChan:
							return false
						default:
							return true
						}
					},
					doReconcile,
				)
				if err != nil {
					klog.Errorf("Failed to reconcile %s: %v", name, err)
				}
			}
			timer.Reset(period)
		}
	}()
	return reconcileCh
}
