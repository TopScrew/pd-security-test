// Copyright 2023 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tsoutil

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/utils/etcdutil"
	"github.com/tikv/pd/pkg/utils/logutil"
	"github.com/tikv/pd/pkg/utils/timerutil"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	maxMergeRequests = 10000
	// DefaultTSOProxyTimeout defines the default timeout value of TSP Proxying
	DefaultTSOProxyTimeout = 3 * time.Second
	// tsoProxyStreamIdleTimeout defines how long Proxy stream will live if no request is received
	tsoProxyStreamIdleTimeout = 5 * time.Minute
)

type tsoResp interface {
	GetTimestamp() *pdpb.Timestamp
}

type tsoRequestProxyQueue struct {
	requestCh chan Request
	ctx       context.Context
	cancel    context.CancelCauseFunc
}

// TSODispatcher dispatches the TSO requests to the corresponding forwarding TSO channels.
type TSODispatcher struct {
	tsoProxyHandleDuration prometheus.Histogram
	tsoProxyBatchSize      prometheus.Histogram

	// dispatchChs is used to dispatch different TSO requests to the corresponding forwarding TSO channels.
	dispatchChs sync.Map // Store as map[string]chan Request
}

// NewTSODispatcher creates and returns a TSODispatcher
func NewTSODispatcher(tsoProxyHandleDuration, tsoProxyBatchSize prometheus.Histogram) *TSODispatcher {
	tsoDispatcher := &TSODispatcher{
		tsoProxyHandleDuration: tsoProxyHandleDuration,
		tsoProxyBatchSize:      tsoProxyBatchSize,
	}
	return tsoDispatcher
}

// DispatchRequest is the entry point for dispatching/forwarding a tso request to the destination host
func (s *TSODispatcher) DispatchRequest(serverCtx context.Context, req Request, tsoProtoFactory ProtoFactory, tsoPrimaryWatchers ...*etcdutil.LoopWatcher) context.Context {
	key := req.getForwardedHost()
	val, loaded := s.dispatchChs.Load(key)
	if !loaded {
		val = &tsoRequestProxyQueue{requestCh: make(chan Request, maxMergeRequests+1)}
		val, loaded = s.dispatchChs.LoadOrStore(key, val)
	}
	tsoQueue := val.(*tsoRequestProxyQueue)
	if !loaded {
		log.Info("start new tso proxy dispatcher", zap.String("forwarded-host", req.getForwardedHost()))
		tsDeadlineCh := make(chan *TSDeadline, 1)
		dispatcherCtx, ctxCancel := context.WithCancelCause(serverCtx)
		tsoQueue.ctx = dispatcherCtx
		tsoQueue.cancel = ctxCancel
		go s.dispatch(tsoQueue, tsoProtoFactory, req.getForwardedHost(), req.getClientConn(), tsDeadlineCh, tsoPrimaryWatchers...)
		go WatchTSDeadline(dispatcherCtx, tsDeadlineCh)
	}
	tsoQueue.requestCh <- req
	return tsoQueue.ctx
}

func (s *TSODispatcher) dispatch(
	tsoQueue *tsoRequestProxyQueue,
	tsoProtoFactory ProtoFactory,
	forwardedHost string,
	clientConn *grpc.ClientConn,
	tsDeadlineCh chan<- *TSDeadline,
	tsoPrimaryWatchers ...*etcdutil.LoopWatcher) {
	defer logutil.LogPanic()
	dispatcherCtx := tsoQueue.ctx
	defer s.dispatchChs.Delete(forwardedHost)

	forwardStream, cancel, err := tsoProtoFactory.createForwardStream(tsoQueue.ctx, clientConn)
	failpoint.Inject("canNotCreateForwardStream", func() {
		cancel()
		err = errors.New("canNotCreateForwardStream")
	})
	if err != nil || forwardStream == nil {
		log.Error("create tso forwarding stream error",
			zap.String("forwarded-host", forwardedHost),
			errs.ZapError(errs.ErrGRPCCreateStream, err))
		if err != nil {
			tsoQueue.cancel(err)
		} else {
			tsoQueue.cancel(errors.New("create tso forwarding stream error: empty stream"))
		}
		return
	}
	defer cancel()

	requests := make([]Request, maxMergeRequests+1)
	needUpdateServicePrimaryAddr := len(tsoPrimaryWatchers) > 0 && tsoPrimaryWatchers[0] != nil
	noProxyRequestsTimer := time.NewTimer(tsoProxyStreamIdleTimeout)
	for {
		noProxyRequestsTimer.Reset(tsoProxyStreamIdleTimeout)
		failpoint.Inject("tsoProxyStreamIdleTimeout", func() {
			noProxyRequestsTimer.Reset(0)
			<-tsoQueue.requestCh // consume the request so that the select below results in the idle case
		})
		select {
		case first := <-tsoQueue.requestCh:
			pendingTSOReqCount := len(tsoQueue.requestCh) + 1
			requests[0] = first
			for i := 1; i < pendingTSOReqCount; i++ {
				requests[i] = <-tsoQueue.requestCh
			}
			done := make(chan struct{})
			dl := NewTSDeadline(DefaultTSOProxyTimeout, done, cancel)
			select {
			case tsDeadlineCh <- dl:
			case <-dispatcherCtx.Done():
				return
			}
			err = s.processRequests(forwardStream, requests[:pendingTSOReqCount], tsoProtoFactory)
			close(done)
			if err != nil {
				log.Error("proxy forward tso error",
					zap.String("forwarded-host", forwardedHost),
					errs.ZapError(errs.ErrGRPCSend, err))
				if needUpdateServicePrimaryAddr && strings.Contains(err.Error(), errs.NotLeaderErr) {
					tsoPrimaryWatchers[0].ForceLoad()
				}
				tsoQueue.cancel(err)
				return
			}
		case <-noProxyRequestsTimer.C:
			log.Info("close tso proxy as it is idle for a while")
			tsoQueue.cancel(errors.New("TSOProxyStreamIdleTimeout"))
			return
		case <-dispatcherCtx.Done():
			return
		}
	}
}

func (s *TSODispatcher) processRequests(forwardStream stream, requests []Request, tsoProtoFactory ProtoFactory) error {
	// Merge the requests
	count := uint32(0)
	for _, request := range requests {
		count += request.getCount()
	}

	start := time.Now()
	resp, err := requests[0].process(forwardStream, count, tsoProtoFactory)
	if err != nil {
		return err
	}
	s.tsoProxyHandleDuration.Observe(time.Since(start).Seconds())
	s.tsoProxyBatchSize.Observe(float64(count))
	// Split the response
	ts := resp.GetTimestamp()
	physical, logical, suffixBits := ts.GetPhysical(), ts.GetLogical(), ts.GetSuffixBits()
	// `logical` is the largest ts's logical part here, we need to do the subtracting before we finish each TSO request.
	// This is different from the logic of client batch, for example, if we have a largest ts whose logical part is 10,
	// count is 5, then the splitting results should be 5 and 10.
	firstLogical := addLogical(logical, -int64(count), suffixBits)
	return s.finishRequest(requests, physical, firstLogical, suffixBits)
}

// Because of the suffix, we need to shift the count before we add it to the logical part.
func addLogical(logical, count int64, suffixBits uint32) int64 {
	return logical + count<<suffixBits
}

func (s *TSODispatcher) finishRequest(requests []Request, physical, firstLogical int64, suffixBits uint32) error {
	countSum := int64(0)
	for i := 0; i < len(requests); i++ {
		newCountSum, err := requests[i].postProcess(countSum, physical, firstLogical, suffixBits)
		if err != nil {
			return err
		}
		countSum = newCountSum
	}
	return nil
}

// TSDeadline is used to watch the deadline of each tso request.
type TSDeadline struct {
	timer  *time.Timer
	done   chan struct{}
	cancel context.CancelFunc
}

// NewTSDeadline creates a new TSDeadline.
func NewTSDeadline(
	timeout time.Duration,
	done chan struct{},
	cancel context.CancelFunc,
) *TSDeadline {
	timer := timerutil.GlobalTimerPool.Get(timeout)
	return &TSDeadline{
		timer:  timer,
		done:   done,
		cancel: cancel,
	}
}

// WatchTSDeadline watches the deadline of each tso request.
func WatchTSDeadline(ctx context.Context, tsDeadlineCh <-chan *TSDeadline) {
	defer logutil.LogPanic()
	for {
		select {
		case d := <-tsDeadlineCh:
			select {
			case <-d.timer.C:
				log.Warn("tso proxy request processing is canceled due to timeout",
					errs.ZapError(errs.ErrProxyTSOTimeout))
				d.cancel()
				timerutil.GlobalTimerPool.Put(d.timer)
			case <-d.done:
				timerutil.GlobalTimerPool.Put(d.timer)
			case <-ctx.Done():
				timerutil.GlobalTimerPool.Put(d.timer)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
