// Copyright 2017 TiKV Project Authors.
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

package server

import (
	"context"
	"fmt"
	"io"
	"path"
	"runtime"
	"runtime/trace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/kvproto/pkg/schedulingpb"
	"github.com/pingcap/kvproto/pkg/tsopb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/core"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/mcs/utils"
	"github.com/tikv/pd/pkg/storage/endpoint"
	"github.com/tikv/pd/pkg/storage/kv"
	"github.com/tikv/pd/pkg/tso"
	"github.com/tikv/pd/pkg/utils/grpcutil"
	"github.com/tikv/pd/pkg/utils/logutil"
	"github.com/tikv/pd/pkg/utils/syncutil"
	"github.com/tikv/pd/pkg/utils/tsoutil"
	"github.com/tikv/pd/pkg/versioninfo"
	"github.com/tikv/pd/server/cluster"
	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	heartbeatSendTimeout          = 5 * time.Second
	maxRetryTimesRequestTSOServer = 3
	retryIntervalRequestTSOServer = 500 * time.Millisecond
	getMinTSFromTSOServerTimeout  = 1 * time.Second
	defaultGRPCDialTimeout        = 3 * time.Second
)

// gRPC errors
var (
	// ErrNotLeader is returned when current server is not the leader and not possible to process request.
	// TODO: work as proxy.
	ErrNotLeader                        = status.Errorf(codes.Unavailable, "not leader")
	ErrNotStarted                       = status.Errorf(codes.Unavailable, "server not started")
	ErrSendHeartbeatTimeout             = status.Errorf(codes.DeadlineExceeded, "send heartbeat timeout")
	ErrNotFoundTSOAddr                  = status.Errorf(codes.NotFound, "not found tso address")
	ErrForwardTSOTimeout                = status.Errorf(codes.DeadlineExceeded, "forward tso request timeout")
	ErrMaxCountTSOProxyRoutinesExceeded = status.Errorf(codes.ResourceExhausted, "max count of concurrent tso proxy routines exceeded")
	ErrTSOProxyRecvFromClientTimeout    = status.Errorf(codes.DeadlineExceeded, "tso proxy timeout when receiving from client; stream closed by server")
	ErrEtcdNotStarted                   = status.Errorf(codes.Unavailable, "server is started, but etcd not started")
)

// GrpcServer wraps Server to provide grpc service.
type GrpcServer struct {
	*Server
	schedulingClient             atomic.Value
	concurrentTSOProxyStreamings atomic.Int32
}

type schedulingClient struct {
	client      schedulingpb.SchedulingClient
	lastPrimary string
}

func (s *schedulingClient) getClient() schedulingpb.SchedulingClient {
	if s == nil {
		return nil
	}
	return s.client
}

func (s *schedulingClient) getPrimaryAddr() string {
	if s == nil {
		return ""
	}
	return s.lastPrimary
}

type request interface {
	GetHeader() *pdpb.RequestHeader
}

type forwardFn func(ctx context.Context, client *grpc.ClientConn) (interface{}, error)

func (s *GrpcServer) unaryMiddleware(ctx context.Context, req request, fn forwardFn) (rsp interface{}, err error) {
	failpoint.Inject("customTimeout", func() {
		time.Sleep(5 * time.Second)
	})
	forwardedHost := grpcutil.GetForwardedHost(ctx)
	if !s.isLocalRequest(forwardedHost) {
		client, err := s.getDelegateClient(ctx, forwardedHost)
		if err != nil {
			return nil, err
		}
		ctx = grpcutil.ResetForwardContext(ctx)
		return fn(ctx, client)
	}
	if err := s.validateRequest(req.GetHeader()); err != nil {
		return nil, err
	}
	return nil, nil
}

// GetClusterInfo implements gRPC PDServer.
func (s *GrpcServer) GetClusterInfo(ctx context.Context, _ *pdpb.GetClusterInfoRequest) (*pdpb.GetClusterInfoResponse, error) {
	// Here we purposely do not check the cluster ID because the client does not know the correct cluster ID
	// at startup and needs to get the cluster ID with the first request (i.e. GetMembers).
	if s.IsClosed() {
		return &pdpb.GetClusterInfoResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrServerNotStarted.FastGenByArgs().Error()),
		}, nil
	}

	var tsoServiceAddrs []string
	svcModes := make([]pdpb.ServiceMode, 0)
	if s.IsAPIServiceMode() {
		svcModes = append(svcModes, pdpb.ServiceMode_API_SVC_MODE)
		tsoServiceAddrs = s.keyspaceGroupManager.GetTSOServiceAddrs()
	} else {
		svcModes = append(svcModes, pdpb.ServiceMode_PD_SVC_MODE)
	}

	return &pdpb.GetClusterInfoResponse{
		Header:       s.header(),
		ServiceModes: svcModes,
		TsoUrls:      tsoServiceAddrs,
	}, nil
}

// GetMinTS implements gRPC PDServer. In PD service mode, it simply returns a timestamp.
// In API service mode, it queries all tso servers and gets the minimum timestamp across
// all keyspace groups.
func (s *GrpcServer) GetMinTS(
	ctx context.Context, request *pdpb.GetMinTSRequest,
) (*pdpb.GetMinTSResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return &pdpb.GetMinTSResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	var (
		minTS *pdpb.Timestamp
		err   error
	)
	if s.IsAPIServiceMode() {
		minTS, err = s.GetMinTSFromTSOService(tso.GlobalDCLocation)
	} else {
		start := time.Now()
		ts, internalErr := s.tsoAllocatorManager.HandleRequest(ctx, tso.GlobalDCLocation, 1)
		if internalErr == nil {
			tsoHandleDuration.Observe(time.Since(start).Seconds())
		}
		minTS = &ts
	}
	if err != nil {
		return &pdpb.GetMinTSResponse{
			Header:    s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
			Timestamp: minTS,
		}, nil
	}

	return &pdpb.GetMinTSResponse{
		Header:    s.header(),
		Timestamp: minTS,
	}, nil
}

// GetMinTSFromTSOService queries all tso servers and gets the minimum timestamp across
// all keyspace groups.
func (s *GrpcServer) GetMinTSFromTSOService(dcLocation string) (*pdpb.Timestamp, error) {
	if s.IsClosed() {
		return nil, ErrNotStarted
	}
	addrs := s.keyspaceGroupManager.GetTSOServiceAddrs()
	if len(addrs) == 0 {
		return &pdpb.Timestamp{}, errs.ErrGetMinTS.FastGenByArgs("no tso servers/pods discovered")
	}

	// Get the minimal timestamp from the TSO servers/pods
	var mutex syncutil.Mutex
	resps := make([]*tsopb.GetMinTSResponse, len(addrs))
	wg := sync.WaitGroup{}
	wg.Add(len(addrs))
	for idx, addr := range addrs {
		go func(idx int, addr string) {
			defer wg.Done()
			resp, err := s.getMinTSFromSingleServer(s.ctx, dcLocation, addr)
			if err != nil || resp == nil {
				log.Warn("failed to get min ts from tso server",
					zap.String("address", addr), zap.Error(err))
				return
			}
			mutex.Lock()
			defer mutex.Unlock()
			resps[idx] = resp
		}(idx, addr)
	}
	wg.Wait()

	// Check the results. The returned minimal timestamp is valid if all the conditions are met:
	// 1. The number of responses is equal to the number of TSO servers/pods.
	// 2. The number of keyspace groups asked is equal to the number of TSO servers/pods.
	// 3. The minimal timestamp is not zero.
	var (
		minTS               *pdpb.Timestamp
		keyspaceGroupsAsked uint32
	)
	if len(resps) == 0 {
		return &pdpb.Timestamp{}, errs.ErrGetMinTS.FastGenByArgs("none of tso server/pod responded")
	}
	emptyTS := &pdpb.Timestamp{}
	keyspaceGroupsTotal := resps[0].KeyspaceGroupsTotal
	for _, resp := range resps {
		if resp.KeyspaceGroupsTotal == 0 {
			return &pdpb.Timestamp{}, errs.ErrGetMinTS.FastGenByArgs("the tso service has no keyspace group")
		}
		if resp.KeyspaceGroupsTotal != keyspaceGroupsTotal {
			return &pdpb.Timestamp{}, errs.ErrGetMinTS.FastGenByArgs(
				"the tso service has inconsistent keyspace group total count")
		}
		keyspaceGroupsAsked += resp.KeyspaceGroupsServing
		if tsoutil.CompareTimestamp(resp.Timestamp, emptyTS) > 0 &&
			(minTS == nil || tsoutil.CompareTimestamp(resp.Timestamp, minTS) < 0) {
			minTS = resp.Timestamp
		}
	}

	if keyspaceGroupsAsked != keyspaceGroupsTotal {
		return &pdpb.Timestamp{}, errs.ErrGetMinTS.FastGenByArgs(
			fmt.Sprintf("can't query all the tso keyspace groups. Asked %d, expected %d",
				keyspaceGroupsAsked, keyspaceGroupsTotal))
	}

	if minTS == nil {
		return &pdpb.Timestamp{}, errs.ErrGetMinTS.FastGenByArgs("the tso service is not ready")
	}

	return minTS, nil
}

func (s *GrpcServer) getMinTSFromSingleServer(
	ctx context.Context, dcLocation, tsoSrvAddr string,
) (*tsopb.GetMinTSResponse, error) {
	cc, err := s.getDelegateClient(s.ctx, tsoSrvAddr)
	if err != nil {
		return nil, errs.ErrClientGetMinTSO.FastGenByArgs(
			fmt.Sprintf("can't connect to tso server %s", tsoSrvAddr))
	}

	cctx, cancel := context.WithTimeout(ctx, getMinTSFromTSOServerTimeout)
	defer cancel()

	resp, err := tsopb.NewTSOClient(cc).GetMinTS(
		cctx, &tsopb.GetMinTSRequest{
			Header: &tsopb.RequestHeader{
				ClusterId: s.ClusterID(),
			},
			DcLocation: dcLocation,
		})
	if err != nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s",
			err, cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMinTSO.Wrap(attachErr).GenWithStackByCause()
	}
	if resp == nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s",
			"no min ts info collected", cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMinTSO.Wrap(attachErr).GenWithStackByCause()
	}
	if resp.GetHeader().GetError() != nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s",
			resp.GetHeader().GetError().String(), cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMinTSO.Wrap(attachErr).GenWithStackByCause()
	}

	return resp, nil
}

// GetMembers implements gRPC PDServer.
func (s *GrpcServer) GetMembers(context.Context, *pdpb.GetMembersRequest) (*pdpb.GetMembersResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.GetMembersResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	// Here we purposely do not check the cluster ID because the client does not know the correct cluster ID
	// at startup and needs to get the cluster ID with the first request (i.e. GetMembers).
	if s.IsClosed() {
		return &pdpb.GetMembersResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrServerNotStarted.FastGenByArgs().Error()),
		}, nil
	}
	members, err := cluster.GetMembers(s.GetClient())
	if err != nil {
		return &pdpb.GetMembersResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	var etcdLeader, pdLeader *pdpb.Member
	leaderID := s.member.GetEtcdLeader()
	for _, m := range members {
		if m.MemberId == leaderID {
			etcdLeader = m
			break
		}
	}

	tsoAllocatorLeaders := make(map[string]*pdpb.Member)
	if !s.IsAPIServiceMode() {
		tsoAllocatorManager := s.GetTSOAllocatorManager()
		tsoAllocatorLeaders, err = tsoAllocatorManager.GetLocalAllocatorLeaders()
	}
	if err != nil {
		return &pdpb.GetMembersResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	leader := s.member.GetLeader()
	for _, m := range members {
		if m.MemberId == leader.GetMemberId() {
			pdLeader = m
			break
		}
	}

	return &pdpb.GetMembersResponse{
		Header:              s.header(),
		Members:             members,
		Leader:              pdLeader,
		EtcdLeader:          etcdLeader,
		TsoAllocatorLeaders: tsoAllocatorLeaders,
	}, nil
}

// Tso implements gRPC PDServer.
func (s *GrpcServer) Tso(stream pdpb.PD_TsoServer) error {
	if s.IsAPIServiceMode() {
		return s.forwardTSO(stream)
	}

	var tsoRequestProxyCtx context.Context
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	for {
		var (
			request *pdpb.TsoRequest
			err     error
		)

		if tsoRequestProxyCtx == nil {
			request, err = stream.Recv()
		} else {
			// if we forward requests to TSO proxy we can't block on the next request in the stream
			// as proxy might fail on the previous request, and we need to return the error to client

			// Create a channel to receive the stream data or error asynchronously
			streamCh := make(chan *pdpb.TsoRequest, 1)
			streamErrCh := make(chan error, 1)
			go func() {
				req, err := stream.Recv()
				if err != nil {
					streamErrCh <- err
				} else {
					streamCh <- req
				}
			}()

			// Wait for either stream data or error from tso proxy
			select {
			case <-tsoRequestProxyCtx.Done():
				err = context.Cause(tsoRequestProxyCtx)
			case err = <-streamErrCh:
			case req := <-streamCh:
				request = req
			}
		}

		if err == io.EOF {
			return nil
		} else if err != nil {
			return errors.WithStack(err)
		}

		// TSO uses leader lease to determine validity. No need to check leader here.
		if s.IsClosed() {
			return ErrNotStarted
		}
		if forwardedHost, err := s.getForwardedHost(ctx, stream.Context()); err != nil {
			return err
		} else if len(forwardedHost) > 0 {
			clientConn, err := s.getDelegateClient(s.ctx, forwardedHost)
			if err != nil {
				return errors.WithStack(err)
			}

			tsoRequest := tsoutil.NewPDProtoRequest(forwardedHost, clientConn, request, stream)
			// don't pass a stream context here as dispatcher serves multiple streams
			tsoRequestProxyCtx = s.tsoDispatcher.DispatchRequest(s.ctx, tsoRequest, s.pdProtoFactory, s.tsoPrimaryWatcher)
			continue
		}

		start := time.Now()

		if request.GetHeader().GetClusterId() != s.clusterID {
			return status.Errorf(codes.FailedPrecondition,
				"mismatch cluster id, need %d but got %d", s.clusterID, request.GetHeader().GetClusterId())
		}
		count := request.GetCount()
		ctx, task := trace.NewTask(ctx, "tso")
		ts, err := s.tsoAllocatorManager.HandleRequest(ctx, request.GetDcLocation(), count)
		task.End()
		if err != nil {
			return status.Errorf(codes.Unknown, err.Error())
		}
		tsoHandleDuration.Observe(time.Since(start).Seconds())
		response := &pdpb.TsoResponse{
			Header:    s.header(),
			Timestamp: &ts,
			Count:     count,
		}
		if err := stream.Send(response); err != nil {
			return errors.WithStack(err)
		}
	}
}

// forwardTSO forward the TSO requests to the TSO service.
func (s *GrpcServer) forwardTSO(stream pdpb.PD_TsoServer) error {
	var (
		server            = &tsoServer{stream: stream}
		forwardStream     tsopb.TSO_TsoClient
		forwardCtx        context.Context
		cancelForward     context.CancelFunc
		lastForwardedHost string
	)
	defer func() {
		s.concurrentTSOProxyStreamings.Add(-1)
		if cancelForward != nil {
			cancelForward()
		}
	}()

	maxConcurrentTSOProxyStreamings := int32(s.GetMaxConcurrentTSOProxyStreamings())
	if maxConcurrentTSOProxyStreamings >= 0 {
		if newCount := s.concurrentTSOProxyStreamings.Add(1); newCount > maxConcurrentTSOProxyStreamings {
			return errors.WithStack(ErrMaxCountTSOProxyRoutinesExceeded)
		}
	}

	tsDeadlineCh := make(chan *tsoutil.TSDeadline, 1)
	go tsoutil.WatchTSDeadline(stream.Context(), tsDeadlineCh)

	for {
		select {
		case <-s.ctx.Done():
			return errors.WithStack(s.ctx.Err())
		case <-stream.Context().Done():
			return stream.Context().Err()
		default:
		}

		request, err := server.Recv(s.GetTSOProxyRecvFromClientTimeout())
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.WithStack(err)
		}
		if request.GetCount() == 0 {
			err = errs.ErrGenerateTimestamp.FastGenByArgs("tso count should be positive")
			return status.Errorf(codes.Unknown, err.Error())
		}

		forwardedHost, ok := s.GetServicePrimaryAddr(stream.Context(), utils.TSOServiceName)
		if !ok || len(forwardedHost) == 0 {
			return errors.WithStack(ErrNotFoundTSOAddr)
		}
		if forwardStream == nil || lastForwardedHost != forwardedHost {
			if cancelForward != nil {
				cancelForward()
			}

			clientConn, err := s.getDelegateClient(s.ctx, forwardedHost)
			if err != nil {
				return errors.WithStack(err)
			}
			forwardStream, forwardCtx, cancelForward, err =
				s.createTSOForwardStream(stream.Context(), clientConn)
			if err != nil {
				return errors.WithStack(err)
			}
			lastForwardedHost = forwardedHost
		}

		tsopbResp, err := s.forwardTSORequestWithDeadLine(
			forwardCtx, cancelForward, forwardStream, request, tsDeadlineCh)
		if err != nil {
			return errors.WithStack(err)
		}

		// The error types defined for tsopb and pdpb are different, so we need to convert them.
		var pdpbErr *pdpb.Error
		tsopbErr := tsopbResp.GetHeader().GetError()
		if tsopbErr != nil {
			if tsopbErr.Type == tsopb.ErrorType_OK {
				pdpbErr = &pdpb.Error{
					Type:    pdpb.ErrorType_OK,
					Message: tsopbErr.GetMessage(),
				}
			} else {
				// TODO: specify FORWARD FAILURE error type instead of UNKNOWN.
				pdpbErr = &pdpb.Error{
					Type:    pdpb.ErrorType_UNKNOWN,
					Message: tsopbErr.GetMessage(),
				}
			}
		}

		response := &pdpb.TsoResponse{
			Header: &pdpb.ResponseHeader{
				ClusterId: tsopbResp.GetHeader().GetClusterId(),
				Error:     pdpbErr,
			},
			Count:     tsopbResp.GetCount(),
			Timestamp: tsopbResp.GetTimestamp(),
		}
		if err := server.Send(response); err != nil {
			return errors.WithStack(err)
		}
	}
}

func (s *GrpcServer) forwardTSORequestWithDeadLine(
	forwardCtx context.Context,
	cancelForward context.CancelFunc,
	forwardStream tsopb.TSO_TsoClient,
	request *pdpb.TsoRequest,
	tsDeadlineCh chan<- *tsoutil.TSDeadline,
) (*tsopb.TsoResponse, error) {
	done := make(chan struct{})
	dl := tsoutil.NewTSDeadline(tsoutil.DefaultTSOProxyTimeout, done, cancelForward)
	select {
	case tsDeadlineCh <- dl:
	case <-forwardCtx.Done():
		return nil, forwardCtx.Err()
	}

	start := time.Now()
	resp, err := s.forwardTSORequest(forwardCtx, request, forwardStream)
	close(done)
	if err != nil {
		if strings.Contains(err.Error(), errs.NotLeaderErr) {
			s.tsoPrimaryWatcher.ForceLoad()
		}
		return nil, err
	}
	tsoProxyBatchSize.Observe(float64(request.GetCount()))
	tsoProxyHandleDuration.Observe(time.Since(start).Seconds())
	return resp, nil
}

func (s *GrpcServer) forwardTSORequest(
	ctx context.Context,
	request *pdpb.TsoRequest,
	forwardStream tsopb.TSO_TsoClient,
) (*tsopb.TsoResponse, error) {
	tsopbReq := &tsopb.TsoRequest{
		Header: &tsopb.RequestHeader{
			ClusterId:       request.GetHeader().GetClusterId(),
			SenderId:        request.GetHeader().GetSenderId(),
			KeyspaceId:      utils.DefaultKeyspaceID,
			KeyspaceGroupId: utils.DefaultKeyspaceGroupID,
		},
		Count:      request.GetCount(),
		DcLocation: request.GetDcLocation(),
	}

	failpoint.Inject("tsoProxySendToTSOTimeout", func() {
		// block until watchDeadline routine cancels the context.
		<-ctx.Done()
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if err := forwardStream.Send(tsopbReq); err != nil {
		return nil, err
	}

	failpoint.Inject("tsoProxyRecvFromTSOTimeout", func() {
		// block until watchDeadline routine cancels the context.
		<-ctx.Done()
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	return forwardStream.Recv()
}

// tsoServer wraps PD_TsoServer to ensure when any error
// occurs on Send() or Recv(), both endpoints will be closed.
type tsoServer struct {
	stream pdpb.PD_TsoServer
	closed int32
}

type pdpbTSORequest struct {
	request *pdpb.TsoRequest
	err     error
}

func (s *tsoServer) Send(m *pdpb.TsoResponse) error {
	if atomic.LoadInt32(&s.closed) == 1 {
		return io.EOF
	}
	done := make(chan error, 1)
	go func() {
		defer logutil.LogPanic()
		failpoint.Inject("tsoProxyFailToSendToClient", func() {
			done <- errors.New("injected error")
			failpoint.Return()
		})
		done <- s.stream.Send(m)
	}()
	timer := time.NewTimer(tsoutil.DefaultTSOProxyTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			atomic.StoreInt32(&s.closed, 1)
		}
		return errors.WithStack(err)
	case <-timer.C:
		atomic.StoreInt32(&s.closed, 1)
		return ErrForwardTSOTimeout
	}
}

func (s *tsoServer) Recv(timeout time.Duration) (*pdpb.TsoRequest, error) {
	if atomic.LoadInt32(&s.closed) == 1 {
		return nil, io.EOF
	}
	failpoint.Inject("tsoProxyRecvFromClientTimeout", func(val failpoint.Value) {
		if customTimeoutInSeconds, ok := val.(int); ok {
			timeout = time.Duration(customTimeoutInSeconds) * time.Second
		}
	})
	requestCh := make(chan *pdpbTSORequest, 1)
	go func() {
		defer logutil.LogPanic()
		request, err := s.stream.Recv()
		requestCh <- &pdpbTSORequest{request: request, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case req := <-requestCh:
		if req.err != nil {
			atomic.StoreInt32(&s.closed, 1)
			return nil, errors.WithStack(req.err)
		}
		return req.request, nil
	case <-timer.C:
		atomic.StoreInt32(&s.closed, 1)
		return nil, ErrTSOProxyRecvFromClientTimeout
	}
}

func (s *GrpcServer) getForwardedHost(ctx, streamCtx context.Context) (forwardedHost string, err error) {
	if s.IsAPIServiceMode() {
		var ok bool
		forwardedHost, ok = s.GetServicePrimaryAddr(ctx, utils.TSOServiceName)
		if !ok || len(forwardedHost) == 0 {
			return "", ErrNotFoundTSOAddr
		}
	} else if fh := grpcutil.GetForwardedHost(streamCtx); !s.isLocalRequest(fh) {
		forwardedHost = fh
	}
	return forwardedHost, nil
}

// Bootstrap implements gRPC PDServer.
func (s *GrpcServer) Bootstrap(ctx context.Context, request *pdpb.BootstrapRequest) (*pdpb.BootstrapResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).Bootstrap(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.BootstrapResponse), nil
	}

	rc := s.GetRaftCluster()
	if rc != nil {
		err := &pdpb.Error{
			Type:    pdpb.ErrorType_ALREADY_BOOTSTRAPPED,
			Message: "cluster is already bootstrapped",
		}
		return &pdpb.BootstrapResponse{
			Header: s.errorHeader(err),
		}, nil
	}

	res, err := s.bootstrapCluster(request)
	if err != nil {
		return &pdpb.BootstrapResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	res.Header = s.header()
	return res, nil
}

// IsBootstrapped implements gRPC PDServer.
func (s *GrpcServer) IsBootstrapped(ctx context.Context, request *pdpb.IsBootstrappedRequest) (*pdpb.IsBootstrappedResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).IsBootstrapped(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.IsBootstrappedResponse), err
	}

	rc := s.GetRaftCluster()
	return &pdpb.IsBootstrappedResponse{
		Header:       s.header(),
		Bootstrapped: rc != nil,
	}, nil
}

// AllocID implements gRPC PDServer.
func (s *GrpcServer) AllocID(ctx context.Context, request *pdpb.AllocIDRequest) (*pdpb.AllocIDResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).AllocID(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.AllocIDResponse), err
	}

	// We can use an allocator for all types ID allocation.
	id, err := s.idAllocator.Alloc()
	if err != nil {
		return &pdpb.AllocIDResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	return &pdpb.AllocIDResponse{
		Header: s.header(),
		Id:     id,
	}, nil
}

// IsSnapshotRecovering implements gRPC PDServer.
func (s *GrpcServer) IsSnapshotRecovering(ctx context.Context, request *pdpb.IsSnapshotRecoveringRequest) (*pdpb.IsSnapshotRecoveringResponse, error) {
	if s.IsClosed() {
		return nil, ErrNotStarted
	}
	// recovering mark is stored in etcd directly, there's no need to forward.
	marked, err := s.Server.IsSnapshotRecovering(ctx)
	if err != nil {
		return &pdpb.IsSnapshotRecoveringResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}
	return &pdpb.IsSnapshotRecoveringResponse{
		Header: s.header(),
		Marked: marked,
	}, nil
}

// GetStore implements gRPC PDServer.
func (s *GrpcServer) GetStore(ctx context.Context, request *pdpb.GetStoreRequest) (*pdpb.GetStoreResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.GetStoreResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetStore(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetStoreResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetStoreResponse{Header: s.notBootstrappedHeader()}, nil
	}

	storeID := request.GetStoreId()
	store := rc.GetStore(storeID)
	if store == nil {
		return &pdpb.GetStoreResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
				fmt.Sprintf("invalid store ID %d, not found", storeID)),
		}, nil
	}
	return &pdpb.GetStoreResponse{
		Header: s.header(),
		Store:  store.GetMeta(),
		Stats:  store.GetStoreStats(),
	}, nil
}

// checkStore returns an error response if the store exists and is in tombstone state.
// It returns nil if it can't get the store.
func checkStore(rc *cluster.RaftCluster, storeID uint64) *pdpb.Error {
	store := rc.GetStore(storeID)
	if store != nil {
		if store.IsRemoved() {
			return &pdpb.Error{
				Type:    pdpb.ErrorType_STORE_TOMBSTONE,
				Message: "store is tombstone",
			}
		}
	}
	return nil
}

// PutStore implements gRPC PDServer.
func (s *GrpcServer) PutStore(ctx context.Context, request *pdpb.PutStoreRequest) (*pdpb.PutStoreResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).PutStore(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.PutStoreResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.PutStoreResponse{Header: s.notBootstrappedHeader()}, nil
	}

	store := request.GetStore()
	if pberr := checkStore(rc, store.GetId()); pberr != nil {
		return &pdpb.PutStoreResponse{
			Header: s.errorHeader(pberr),
		}, nil
	}

	// NOTE: can be removed when placement rules feature is enabled by default.
	if !s.GetConfig().Replication.EnablePlacementRules && core.IsStoreContainLabel(store, core.EngineKey, core.EngineTiFlash) {
		return &pdpb.PutStoreResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
				"placement rules is disabled"),
		}, nil
	}

	if err := rc.PutStore(store); err != nil {
		return &pdpb.PutStoreResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	log.Info("put store ok", zap.Stringer("store", store))
	CheckPDVersion(s.persistOptions)

	return &pdpb.PutStoreResponse{
		Header:            s.header(),
		ReplicationStatus: rc.GetReplicationMode().GetReplicationStatus(),
	}, nil
}

// GetAllStores implements gRPC PDServer.
func (s *GrpcServer) GetAllStores(ctx context.Context, request *pdpb.GetAllStoresRequest) (*pdpb.GetAllStoresResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.GetAllStoresResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetAllStores(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetAllStoresResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetAllStoresResponse{Header: s.notBootstrappedHeader()}, nil
	}

	// Don't return tombstone stores.
	var stores []*metapb.Store
	if request.GetExcludeTombstoneStores() {
		for _, store := range rc.GetMetaStores() {
			if store.GetNodeState() != metapb.NodeState_Removed {
				stores = append(stores, store)
			}
		}
	} else {
		stores = rc.GetMetaStores()
	}

	return &pdpb.GetAllStoresResponse{
		Header: s.header(),
		Stores: stores,
	}, nil
}

// StoreHeartbeat implements gRPC PDServer.
func (s *GrpcServer) StoreHeartbeat(ctx context.Context, request *pdpb.StoreHeartbeatRequest) (*pdpb.StoreHeartbeatResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.StoreHeartbeatResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).StoreHeartbeat(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.StoreHeartbeatResponse), err
	}

	if request.GetStats() == nil {
		return nil, errors.Errorf("invalid store heartbeat command, but %v", request)
	}
	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.StoreHeartbeatResponse{Header: s.notBootstrappedHeader()}, nil
	}

	if pberr := checkStore(rc, request.GetStats().GetStoreId()); pberr != nil {
		return &pdpb.StoreHeartbeatResponse{
			Header: s.errorHeader(pberr),
		}, nil
	}
	storeID := request.GetStats().GetStoreId()
	store := rc.GetStore(storeID)
	if store == nil {
		return &pdpb.StoreHeartbeatResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
				fmt.Sprintf("store %v not found", storeID)),
		}, nil
	}

	resp := &pdpb.StoreHeartbeatResponse{Header: s.header()}
	// Bypass stats handling if the store report for unsafe recover is not empty.
	if request.GetStoreReport() == nil {
		storeAddress := store.GetAddress()
		storeLabel := strconv.FormatUint(storeID, 10)
		start := time.Now()

		err := rc.HandleStoreHeartbeat(request, resp)
		if err != nil {
			return &pdpb.StoreHeartbeatResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
					err.Error()),
			}, nil
		}

		s.handleDamagedStore(request.GetStats())
		storeHeartbeatHandleDuration.WithLabelValues(storeAddress, storeLabel).Observe(time.Since(start).Seconds())
		if s.IsAPIServiceMode() {
			s.updateSchedulingClient(ctx)
			if s.schedulingClient.Load() != nil {
				req := &schedulingpb.StoreHeartbeatRequest{
					Header: &schedulingpb.RequestHeader{
						ClusterId: request.GetHeader().GetClusterId(),
						SenderId:  request.GetHeader().GetSenderId(),
					},
					Stats: request.GetStats(),
				}
				if _, err := s.schedulingClient.Load().(*schedulingClient).getClient().StoreHeartbeat(ctx, req); err != nil {
					// reset to let it be updated in the next request
					s.schedulingClient.Store(&schedulingClient{})
				}
			}
		}
	}

	if status := request.GetDrAutosyncStatus(); status != nil {
		rc.GetReplicationMode().UpdateStoreDRStatus(request.GetStats().GetStoreId(), status)
	}

	resp.ReplicationStatus = rc.GetReplicationMode().GetReplicationStatus()
	resp.ClusterVersion = rc.GetClusterVersion()
	rc.GetUnsafeRecoveryController().HandleStoreHeartbeat(request, resp)

	return resp, nil
}

func (s *GrpcServer) updateSchedulingClient(ctx context.Context) {
	forwardedHost, _ := s.GetServicePrimaryAddr(ctx, utils.SchedulingServiceName)
	pre := s.schedulingClient.Load()
	if forwardedHost != "" && ((pre == nil) || (pre != nil && forwardedHost != pre.(*schedulingClient).getPrimaryAddr())) {
		client, err := s.getDelegateClient(ctx, forwardedHost)
		if err != nil {
			log.Error("get delegate client failed", zap.Error(err))
		}
		s.schedulingClient.Store(&schedulingClient{
			client:      schedulingpb.NewSchedulingClient(client),
			lastPrimary: forwardedHost,
		})
	}
}

// bucketHeartbeatServer wraps PD_ReportBucketsServer to ensure when any error
// occurs on SendAndClose() or Recv(), both endpoints will be closed.
type bucketHeartbeatServer struct {
	stream pdpb.PD_ReportBucketsServer
	closed int32
}

func (b *bucketHeartbeatServer) Send(bucket *pdpb.ReportBucketsResponse) error {
	if atomic.LoadInt32(&b.closed) == 1 {
		return status.Errorf(codes.Canceled, "stream is closed")
	}
	done := make(chan error, 1)
	go func() {
		defer logutil.LogPanic()
		done <- b.stream.SendAndClose(bucket)
	}()
	timer := time.NewTimer(heartbeatSendTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			atomic.StoreInt32(&b.closed, 1)
		}
		return err
	case <-timer.C:
		atomic.StoreInt32(&b.closed, 1)
		return ErrSendHeartbeatTimeout
	}
}

func (b *bucketHeartbeatServer) Recv() (*pdpb.ReportBucketsRequest, error) {
	if atomic.LoadInt32(&b.closed) == 1 {
		return nil, io.EOF
	}
	req, err := b.stream.Recv()
	if err != nil {
		atomic.StoreInt32(&b.closed, 1)
		return nil, errors.WithStack(err)
	}
	return req, nil
}

// heartbeatServer wraps PD_RegionHeartbeatServer to ensure when any error
// occurs on Send() or Recv(), both endpoints will be closed.
type heartbeatServer struct {
	stream pdpb.PD_RegionHeartbeatServer
	closed int32
}

func (s *heartbeatServer) Send(m core.RegionHeartbeatResponse) error {
	if atomic.LoadInt32(&s.closed) == 1 {
		return io.EOF
	}
	done := make(chan error, 1)
	go func() {
		defer logutil.LogPanic()
		done <- s.stream.Send(m.(*pdpb.RegionHeartbeatResponse))
	}()
	timer := time.NewTimer(heartbeatSendTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			atomic.StoreInt32(&s.closed, 1)
		}
		return errors.WithStack(err)
	case <-timer.C:
		atomic.StoreInt32(&s.closed, 1)
		return ErrSendHeartbeatTimeout
	}
}

func (s *heartbeatServer) Recv() (*pdpb.RegionHeartbeatRequest, error) {
	if atomic.LoadInt32(&s.closed) == 1 {
		return nil, io.EOF
	}
	req, err := s.stream.Recv()
	if err != nil {
		atomic.StoreInt32(&s.closed, 1)
		return nil, errors.WithStack(err)
	}
	return req, nil
}

// ReportBuckets implements gRPC PDServer
func (s *GrpcServer) ReportBuckets(stream pdpb.PD_ReportBucketsServer) error {
	var (
		server            = &bucketHeartbeatServer{stream: stream}
		forwardStream     pdpb.PD_ReportBucketsClient
		cancel            context.CancelFunc
		lastForwardedHost string
		errCh             chan error
	)
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	for {
		request, err := server.Recv()
		failpoint.Inject("grpcClientClosed", func() {
			err = status.Error(codes.Canceled, "grpc client closed")
			request = nil
		})
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.WithStack(err)
		}
		forwardedHost := grpcutil.GetForwardedHost(stream.Context())
		failpoint.Inject("grpcClientClosed", func() {
			forwardedHost = s.GetMember().Member().GetClientUrls()[0]
		})
		if !s.isLocalRequest(forwardedHost) {
			if forwardStream == nil || lastForwardedHost != forwardedHost {
				if cancel != nil {
					cancel()
				}
				client, err := s.getDelegateClient(s.ctx, forwardedHost)
				if err != nil {
					return err
				}
				log.Info("create bucket report forward stream", zap.String("forwarded-host", forwardedHost))
				forwardStream, cancel, err = s.createReportBucketsForwardStream(client)
				if err != nil {
					return err
				}
				lastForwardedHost = forwardedHost
				errCh = make(chan error, 1)
				go forwardReportBucketClientToServer(forwardStream, server, errCh)
			}
			if err := forwardStream.Send(request); err != nil {
				return errors.WithStack(err)
			}

			select {
			case err := <-errCh:
				return err
			default:
			}
			continue
		}
		rc := s.GetRaftCluster()
		if rc == nil {
			resp := &pdpb.ReportBucketsResponse{
				Header: s.notBootstrappedHeader(),
			}
			err := server.Send(resp)
			return errors.WithStack(err)
		}
		if err := s.validateRequest(request.GetHeader()); err != nil {
			return err
		}
		buckets := request.GetBuckets()
		if buckets == nil || len(buckets.Keys) == 0 {
			continue
		}
		store := rc.GetLeaderStoreByRegionID(buckets.GetRegionId())
		if store == nil {
			return errors.Errorf("the store of the bucket in region %v is not found ", buckets.GetRegionId())
		}
		storeLabel := strconv.FormatUint(store.GetID(), 10)
		storeAddress := store.GetAddress()
		bucketReportCounter.WithLabelValues(storeAddress, storeLabel, "report", "recv").Inc()

		start := time.Now()
		err = rc.HandleReportBuckets(buckets)
		if err != nil {
			bucketReportCounter.WithLabelValues(storeAddress, storeLabel, "report", "err").Inc()
			continue
		}
		bucketReportInterval.WithLabelValues(storeAddress, storeLabel).Observe(float64(buckets.GetPeriodInMs() / 1000))
		bucketReportLatency.WithLabelValues(storeAddress, storeLabel).Observe(time.Since(start).Seconds())
		bucketReportCounter.WithLabelValues(storeAddress, storeLabel, "report", "ok").Inc()
	}
}

// RegionHeartbeat implements gRPC PDServer.
func (s *GrpcServer) RegionHeartbeat(stream pdpb.PD_RegionHeartbeatServer) error {
	var (
		server            = &heartbeatServer{stream: stream}
		flowRoundOption   = core.WithFlowRoundByDigit(s.persistOptions.GetPDServerConfig().FlowRoundByDigit)
		forwardStream     pdpb.PD_RegionHeartbeatClient
		cancel            context.CancelFunc
		lastForwardedHost string
		lastBind          time.Time
		errCh             chan error
		schedulingStream  schedulingpb.Scheduling_RegionHeartbeatClient
		cancel1           context.CancelFunc
		lastPrimaryAddr   string
	)
	defer func() {
		// cancel the forward stream
		if cancel != nil {
			cancel()
		}
	}()

	for {
		request, err := server.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.WithStack(err)
		}

		forwardedHost := grpcutil.GetForwardedHost(stream.Context())
		if !s.isLocalRequest(forwardedHost) {
			if forwardStream == nil || lastForwardedHost != forwardedHost {
				if cancel != nil {
					cancel()
				}
				client, err := s.getDelegateClient(s.ctx, forwardedHost)
				if err != nil {
					return err
				}
				log.Info("create region heartbeat forward stream", zap.String("forwarded-host", forwardedHost))
				forwardStream, cancel, err = s.createHeartbeatForwardStream(client)
				if err != nil {
					return err
				}
				lastForwardedHost = forwardedHost
				errCh = make(chan error, 1)
				go forwardRegionHeartbeatClientToServer(forwardStream, server, errCh)
			}
			if err := forwardStream.Send(request); err != nil {
				return errors.WithStack(err)
			}

			select {
			case err := <-errCh:
				return err
			default:
			}
			continue
		}

		rc := s.GetRaftCluster()
		if rc == nil {
			resp := &pdpb.RegionHeartbeatResponse{
				Header: s.notBootstrappedHeader(),
			}
			err := server.Send(resp)
			return errors.WithStack(err)
		}

		if err = s.validateRequest(request.GetHeader()); err != nil {
			return err
		}

		storeID := request.GetLeader().GetStoreId()
		storeLabel := strconv.FormatUint(storeID, 10)
		store := rc.GetStore(storeID)
		if store == nil {
			return errors.Errorf("invalid store ID %d, not found", storeID)
		}
		storeAddress := store.GetAddress()

		regionHeartbeatCounter.WithLabelValues(storeAddress, storeLabel, "report", "recv").Inc()
		regionHeartbeatLatency.WithLabelValues(storeAddress, storeLabel).Observe(float64(time.Now().Unix()) - float64(request.GetInterval().GetEndTimestamp()))

		if time.Since(lastBind) > s.cfg.HeartbeatStreamBindInterval.Duration {
			regionHeartbeatCounter.WithLabelValues(storeAddress, storeLabel, "report", "bind").Inc()
			s.hbStreams.BindStream(storeID, server)
			// refresh FlowRoundByDigit
			flowRoundOption = core.WithFlowRoundByDigit(s.persistOptions.GetPDServerConfig().FlowRoundByDigit)
			lastBind = time.Now()
		}

		region := core.RegionFromHeartbeat(request, flowRoundOption)
		if region.GetLeader() == nil {
			log.Error("invalid request, the leader is nil", zap.Reflect("request", request), errs.ZapError(errs.ErrLeaderNil))
			regionHeartbeatCounter.WithLabelValues(storeAddress, storeLabel, "report", "invalid-leader").Inc()
			msg := fmt.Sprintf("invalid request leader, %v", request)
			s.hbStreams.SendErr(pdpb.ErrorType_UNKNOWN, msg, request.GetLeader())
			continue
		}
		if region.GetID() == 0 {
			regionHeartbeatCounter.WithLabelValues(storeAddress, storeLabel, "report", "invalid-region").Inc()
			msg := fmt.Sprintf("invalid request region, %v", request)
			s.hbStreams.SendErr(pdpb.ErrorType_UNKNOWN, msg, request.GetLeader())
			continue
		}

		// If the region peer count is 0, then we should not handle this.
		if len(region.GetPeers()) == 0 {
			log.Warn("invalid region, zero region peer count",
				logutil.ZapRedactStringer("region-meta", core.RegionToHexMeta(region.GetMeta())))
			regionHeartbeatCounter.WithLabelValues(storeAddress, storeLabel, "report", "no-peer").Inc()
			msg := fmt.Sprintf("invalid region, zero region peer count: %v", logutil.RedactStringer(core.RegionToHexMeta(region.GetMeta())))
			s.hbStreams.SendErr(pdpb.ErrorType_UNKNOWN, msg, request.GetLeader())
			continue
		}
		start := time.Now()

		err = rc.HandleRegionHeartbeat(region)
		if err != nil {
			regionHeartbeatCounter.WithLabelValues(storeAddress, storeLabel, "report", "err").Inc()
			msg := err.Error()
			s.hbStreams.SendErr(pdpb.ErrorType_UNKNOWN, msg, request.GetLeader())
			continue
		}

		if s.IsAPIServiceMode() {
			ctx := stream.Context()
			primaryAddr, _ := s.GetServicePrimaryAddr(ctx, utils.SchedulingServiceName)
			if schedulingStream == nil || lastPrimaryAddr != primaryAddr {
				if cancel1 != nil {
					cancel1()
				}
				client, err := s.getDelegateClient(ctx, primaryAddr)
				if err != nil {
					log.Error("get delegate client failed", zap.Error(err))
				}

				log.Info("create region heartbeat forward stream", zap.String("forwarded-host", primaryAddr))
				schedulingStream, cancel1, err = s.createSchedulingStream(client)
				if err != nil {
					log.Error("create region heartbeat forward stream failed", zap.Error(err))
				} else {
					lastPrimaryAddr = primaryAddr
					errCh = make(chan error, 1)
					go forwardSchedulingToServer(schedulingStream, server, errCh)
				}
			}
			if schedulingStream != nil {
				req := &schedulingpb.RegionHeartbeatRequest{
					Header: &schedulingpb.RequestHeader{
						ClusterId: request.GetHeader().GetClusterId(),
						SenderId:  request.GetHeader().GetSenderId(),
					},
					Region:          request.GetRegion(),
					Leader:          request.GetLeader(),
					DownPeers:       request.GetDownPeers(),
					PendingPeers:    request.GetPendingPeers(),
					BytesWritten:    request.GetBytesWritten(),
					BytesRead:       request.GetBytesRead(),
					KeysWritten:     request.GetKeysWritten(),
					KeysRead:        request.GetKeysRead(),
					ApproximateSize: request.GetApproximateSize(),
					ApproximateKeys: request.GetApproximateKeys(),
					Interval:        request.GetInterval(),
					Term:            request.GetTerm(),
					QueryStats:      request.GetQueryStats(),
				}
				if err := schedulingStream.Send(req); err != nil {
					log.Error("forward region heartbeat failed", zap.Error(err))
				}
			}
		}

		regionHeartbeatHandleDuration.WithLabelValues(storeAddress, storeLabel).Observe(time.Since(start).Seconds())
		regionHeartbeatCounter.WithLabelValues(storeAddress, storeLabel, "report", "ok").Inc()
	}
}

// GetRegion implements gRPC PDServer.
func (s *GrpcServer) GetRegion(ctx context.Context, request *pdpb.GetRegionRequest) (*pdpb.GetRegionResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.GetRegionResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetRegion(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetRegionResponse), nil
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}
	region := rc.GetRegionByKey(request.GetRegionKey())
	if region == nil {
		return &pdpb.GetRegionResponse{Header: s.header()}, nil
	}
	var buckets *metapb.Buckets
	if rc.GetStoreConfig().IsEnableRegionBucket() && request.GetNeedBuckets() {
		buckets = region.GetBuckets()
	}
	return &pdpb.GetRegionResponse{
		Header:       s.header(),
		Region:       region.GetMeta(),
		Leader:       region.GetLeader(),
		DownPeers:    region.GetDownPeers(),
		PendingPeers: region.GetPendingPeers(),
		Buckets:      buckets,
	}, nil
}

// GetPrevRegion implements gRPC PDServer
func (s *GrpcServer) GetPrevRegion(ctx context.Context, request *pdpb.GetRegionRequest) (*pdpb.GetRegionResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.GetRegionResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetPrevRegion(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetRegionResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}

	region := rc.GetPrevRegionByKey(request.GetRegionKey())
	if region == nil {
		return &pdpb.GetRegionResponse{Header: s.header()}, nil
	}
	var buckets *metapb.Buckets
	if rc.GetStoreConfig().IsEnableRegionBucket() && request.GetNeedBuckets() {
		buckets = region.GetBuckets()
	}
	return &pdpb.GetRegionResponse{
		Header:       s.header(),
		Region:       region.GetMeta(),
		Leader:       region.GetLeader(),
		DownPeers:    region.GetDownPeers(),
		PendingPeers: region.GetPendingPeers(),
		Buckets:      buckets,
	}, nil
}

// GetRegionByID implements gRPC PDServer.
func (s *GrpcServer) GetRegionByID(ctx context.Context, request *pdpb.GetRegionByIDRequest) (*pdpb.GetRegionResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.GetRegionResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetRegionByID(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetRegionResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}
	region := rc.GetRegion(request.GetRegionId())
	if region == nil {
		return &pdpb.GetRegionResponse{Header: s.header()}, nil
	}
	var buckets *metapb.Buckets
	if rc.GetStoreConfig().IsEnableRegionBucket() && request.GetNeedBuckets() {
		buckets = region.GetBuckets()
	}
	return &pdpb.GetRegionResponse{
		Header:       s.header(),
		Region:       region.GetMeta(),
		Leader:       region.GetLeader(),
		DownPeers:    region.GetDownPeers(),
		PendingPeers: region.GetPendingPeers(),
		Buckets:      buckets,
	}, nil
}

// ScanRegions implements gRPC PDServer.
func (s *GrpcServer) ScanRegions(ctx context.Context, request *pdpb.ScanRegionsRequest) (*pdpb.ScanRegionsResponse, error) {
	if s.GetServiceMiddlewarePersistOptions().IsGRPCRateLimitEnabled() {
		fName := currentFunction()
		limiter := s.GetGRPCRateLimiter()
		if limiter.Allow(fName) {
			defer limiter.Release(fName)
		} else {
			return &pdpb.ScanRegionsResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, errs.ErrRateLimitExceeded.FastGenByArgs().Error()),
			}, nil
		}
	}
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).ScanRegions(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.ScanRegionsResponse), nil
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.ScanRegionsResponse{Header: s.notBootstrappedHeader()}, nil
	}
	regions := rc.ScanRegions(request.GetStartKey(), request.GetEndKey(), int(request.GetLimit()))
	resp := &pdpb.ScanRegionsResponse{Header: s.header()}
	for _, r := range regions {
		leader := r.GetLeader()
		if leader == nil {
			leader = &metapb.Peer{}
		}
		// Set RegionMetas and Leaders to make it compatible with old client.
		resp.RegionMetas = append(resp.RegionMetas, r.GetMeta())
		resp.Leaders = append(resp.Leaders, leader)
		resp.Regions = append(resp.Regions, &pdpb.Region{
			Region:       r.GetMeta(),
			Leader:       leader,
			DownPeers:    r.GetDownPeers(),
			PendingPeers: r.GetPendingPeers(),
		})
	}
	return resp, nil
}

// AskSplit implements gRPC PDServer.
func (s *GrpcServer) AskSplit(ctx context.Context, request *pdpb.AskSplitRequest) (*pdpb.AskSplitResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).AskSplit(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.AskSplitResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.AskSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	if request.GetRegion() == nil {
		return &pdpb.AskSplitResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_REGION_NOT_FOUND,
				"missing region for split"),
		}, nil
	}
	req := &pdpb.AskSplitRequest{
		Region: request.Region,
	}
	split, err := rc.HandleAskSplit(req)
	if err != nil {
		return &pdpb.AskSplitResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	return &pdpb.AskSplitResponse{
		Header:      s.header(),
		NewRegionId: split.NewRegionId,
		NewPeerIds:  split.NewPeerIds,
	}, nil
}

// AskBatchSplit implements gRPC PDServer.
func (s *GrpcServer) AskBatchSplit(ctx context.Context, request *pdpb.AskBatchSplitRequest) (*pdpb.AskBatchSplitResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).AskBatchSplit(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.AskBatchSplitResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.AskBatchSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}

	if !versioninfo.IsFeatureSupported(rc.GetOpts().GetClusterVersion(), versioninfo.BatchSplit) {
		return &pdpb.AskBatchSplitResponse{Header: s.incompatibleVersion("batch_split")}, nil
	}
	if request.GetRegion() == nil {
		return &pdpb.AskBatchSplitResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_REGION_NOT_FOUND,
				"missing region for split"),
		}, nil
	}
	req := &pdpb.AskBatchSplitRequest{
		Region:     request.Region,
		SplitCount: request.SplitCount,
	}
	split, err := rc.HandleAskBatchSplit(req)
	if err != nil {
		return &pdpb.AskBatchSplitResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	return &pdpb.AskBatchSplitResponse{
		Header: s.header(),
		Ids:    split.Ids,
	}, nil
}

// ReportSplit implements gRPC PDServer.
func (s *GrpcServer) ReportSplit(ctx context.Context, request *pdpb.ReportSplitRequest) (*pdpb.ReportSplitResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).ReportSplit(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.ReportSplitResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.ReportSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	_, err := rc.HandleReportSplit(request)
	if err != nil {
		return &pdpb.ReportSplitResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}

	return &pdpb.ReportSplitResponse{
		Header: s.header(),
	}, nil
}

// ReportBatchSplit implements gRPC PDServer.
func (s *GrpcServer) ReportBatchSplit(ctx context.Context, request *pdpb.ReportBatchSplitRequest) (*pdpb.ReportBatchSplitResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).ReportBatchSplit(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.ReportBatchSplitResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.ReportBatchSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	_, err := rc.HandleBatchReportSplit(request)
	if err != nil {
		return &pdpb.ReportBatchSplitResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
				err.Error()),
		}, nil
	}

	return &pdpb.ReportBatchSplitResponse{
		Header: s.header(),
	}, nil
}

// GetClusterConfig implements gRPC PDServer.
func (s *GrpcServer) GetClusterConfig(ctx context.Context, request *pdpb.GetClusterConfigRequest) (*pdpb.GetClusterConfigResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetClusterConfig(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetClusterConfigResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetClusterConfigResponse{Header: s.notBootstrappedHeader()}, nil
	}
	return &pdpb.GetClusterConfigResponse{
		Header:  s.header(),
		Cluster: rc.GetMetaCluster(),
	}, nil
}

// PutClusterConfig implements gRPC PDServer.
func (s *GrpcServer) PutClusterConfig(ctx context.Context, request *pdpb.PutClusterConfigRequest) (*pdpb.PutClusterConfigResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).PutClusterConfig(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.PutClusterConfigResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.PutClusterConfigResponse{Header: s.notBootstrappedHeader()}, nil
	}
	conf := request.GetCluster()
	if err := rc.PutMetaCluster(conf); err != nil {
		return &pdpb.PutClusterConfigResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
				err.Error()),
		}, nil
	}

	log.Info("put cluster config ok", zap.Reflect("config", conf))

	return &pdpb.PutClusterConfigResponse{
		Header: s.header(),
	}, nil
}

// ScatterRegion implements gRPC PDServer.
func (s *GrpcServer) ScatterRegion(ctx context.Context, request *pdpb.ScatterRegionRequest) (*pdpb.ScatterRegionResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).ScatterRegion(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.ScatterRegionResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.ScatterRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}

	if len(request.GetRegionsId()) > 0 {
		percentage, err := scatterRegions(rc, request.GetRegionsId(), request.GetGroup(), int(request.GetRetryLimit()), request.GetSkipStoreLimit())
		if err != nil {
			return nil, err
		}
		return &pdpb.ScatterRegionResponse{
			Header:             s.header(),
			FinishedPercentage: uint64(percentage),
		}, nil
	}
	// TODO: Deprecate it use `request.GetRegionsID`.
	//nolint
	region := rc.GetRegion(request.GetRegionId())
	if region == nil {
		if request.GetRegion() == nil {
			//nolint
			return &pdpb.ScatterRegionResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_REGION_NOT_FOUND,
					"region %d not found"),
			}, nil
		}
		region = core.NewRegionInfo(request.GetRegion(), request.GetLeader())
	}

	op, err := rc.GetRegionScatterer().Scatter(region, request.GetGroup(), request.GetSkipStoreLimit())
	if err != nil {
		return nil, err
	}

	if op != nil {
		if !rc.GetOperatorController().AddOperator(op) {
			return &pdpb.ScatterRegionResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
					"operator canceled because cannot add an operator to the execute queue"),
			}, nil
		}
	}

	return &pdpb.ScatterRegionResponse{
		Header:             s.header(),
		FinishedPercentage: 100,
	}, nil
}

// GetGCSafePoint implements gRPC PDServer.
func (s *GrpcServer) GetGCSafePoint(ctx context.Context, request *pdpb.GetGCSafePointRequest) (*pdpb.GetGCSafePointResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetGCSafePoint(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetGCSafePointResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetGCSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}

	safePoint, err := s.gcSafePointManager.LoadGCSafePoint()
	if err != nil {
		return nil, err
	}

	return &pdpb.GetGCSafePointResponse{
		Header:    s.header(),
		SafePoint: safePoint,
	}, nil
}

// SyncRegions syncs the regions.
func (s *GrpcServer) SyncRegions(stream pdpb.PD_SyncRegionsServer) error {
	if s.IsClosed() || s.cluster == nil {
		return ErrNotStarted
	}
	ctx := s.cluster.Context()
	if ctx == nil {
		return ErrNotStarted
	}
	return s.cluster.GetRegionSyncer().Sync(ctx, stream)
}

// UpdateGCSafePoint implements gRPC PDServer.
func (s *GrpcServer) UpdateGCSafePoint(ctx context.Context, request *pdpb.UpdateGCSafePointRequest) (*pdpb.UpdateGCSafePointResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).UpdateGCSafePoint(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.UpdateGCSafePointResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.UpdateGCSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}

	newSafePoint := request.GetSafePoint()
	oldSafePoint, err := s.gcSafePointManager.UpdateGCSafePoint(newSafePoint)
	if err != nil {
		return nil, err
	}

	if newSafePoint > oldSafePoint {
		log.Info("updated gc safe point",
			zap.Uint64("safe-point", newSafePoint))
	} else if newSafePoint < oldSafePoint {
		log.Warn("trying to update gc safe point",
			zap.Uint64("old-safe-point", oldSafePoint),
			zap.Uint64("new-safe-point", newSafePoint))
		newSafePoint = oldSafePoint
	}

	return &pdpb.UpdateGCSafePointResponse{
		Header:       s.header(),
		NewSafePoint: newSafePoint,
	}, nil
}

// UpdateServiceGCSafePoint update the safepoint for specific service
func (s *GrpcServer) UpdateServiceGCSafePoint(ctx context.Context, request *pdpb.UpdateServiceGCSafePointRequest) (*pdpb.UpdateServiceGCSafePointResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).UpdateServiceGCSafePoint(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.UpdateServiceGCSafePointResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.UpdateServiceGCSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}
	var storage endpoint.GCSafePointStorage = s.storage
	if request.TTL <= 0 {
		if err := storage.RemoveServiceGCSafePoint(string(request.ServiceId)); err != nil {
			return nil, err
		}
	}
	nowTSO, err := s.getGlobalTSO(ctx)
	if err != nil {
		return nil, err
	}
	now, _ := tsoutil.ParseTimestamp(nowTSO)
	serviceID := string(request.ServiceId)
	min, updated, err := s.gcSafePointManager.UpdateServiceGCSafePoint(serviceID, request.GetSafePoint(), request.GetTTL(), now)
	if err != nil {
		return nil, err
	}
	if updated {
		log.Info("update service GC safe point",
			zap.String("service-id", serviceID),
			zap.Int64("expire-at", now.Unix()+request.GetTTL()),
			zap.Uint64("safepoint", request.GetSafePoint()))
	}
	return &pdpb.UpdateServiceGCSafePointResponse{
		Header:       s.header(),
		ServiceId:    []byte(min.ServiceID),
		TTL:          min.ExpiredAt - now.Unix(),
		MinSafePoint: min.SafePoint,
	}, nil
}

// GetOperator gets information about the operator belonging to the specify region.
func (s *GrpcServer) GetOperator(ctx context.Context, request *pdpb.GetOperatorRequest) (*pdpb.GetOperatorResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetOperator(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetOperatorResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.GetOperatorResponse{Header: s.notBootstrappedHeader()}, nil
	}

	opController := rc.GetOperatorController()
	requestID := request.GetRegionId()
	r := opController.GetOperatorStatus(requestID)
	if r == nil {
		header := s.errorHeader(&pdpb.Error{
			Type:    pdpb.ErrorType_REGION_NOT_FOUND,
			Message: "Not Found",
		})
		return &pdpb.GetOperatorResponse{Header: header}, nil
	}

	return &pdpb.GetOperatorResponse{
		Header:   s.header(),
		RegionId: requestID,
		Desc:     []byte(r.Desc()),
		Kind:     []byte(r.Kind().String()),
		Status:   r.Status,
	}, nil
}

// validateRequest checks if Server is leader and clusterID is matched.
// TODO: Call it in gRPC interceptor.
func (s *GrpcServer) validateRequest(header *pdpb.RequestHeader) error {
	if s.IsClosed() || !s.member.IsLeader() {
		return ErrNotLeader
	}
	if header.GetClusterId() != s.clusterID {
		return status.Errorf(codes.FailedPrecondition, "mismatch cluster id, need %d but got %d", s.clusterID, header.GetClusterId())
	}
	return nil
}

func (s *GrpcServer) header() *pdpb.ResponseHeader {
	if s.clusterID == 0 {
		return s.wrapErrorToHeader(pdpb.ErrorType_NOT_BOOTSTRAPPED, "cluster id is not ready")
	}
	return &pdpb.ResponseHeader{ClusterId: s.clusterID}
}

func (s *GrpcServer) wrapErrorToHeader(errorType pdpb.ErrorType, message string) *pdpb.ResponseHeader {
	return s.errorHeader(&pdpb.Error{
		Type:    errorType,
		Message: message,
	})
}

func (s *GrpcServer) errorHeader(err *pdpb.Error) *pdpb.ResponseHeader {
	return &pdpb.ResponseHeader{
		ClusterId: s.clusterID,
		Error:     err,
	}
}

func (s *GrpcServer) notBootstrappedHeader() *pdpb.ResponseHeader {
	return s.errorHeader(&pdpb.Error{
		Type:    pdpb.ErrorType_NOT_BOOTSTRAPPED,
		Message: "cluster is not bootstrapped",
	})
}

func (s *GrpcServer) incompatibleVersion(tag string) *pdpb.ResponseHeader {
	msg := fmt.Sprintf("%s incompatible with current cluster version %s", tag, s.persistOptions.GetClusterVersion())
	return s.errorHeader(&pdpb.Error{
		Type:    pdpb.ErrorType_INCOMPATIBLE_VERSION,
		Message: msg,
	})
}

func (s *GrpcServer) invalidValue(msg string) *pdpb.ResponseHeader {
	return s.errorHeader(&pdpb.Error{
		Type:    pdpb.ErrorType_INVALID_VALUE,
		Message: msg,
	})
}

// Only used for the TestLocalAllocatorLeaderChange.
var mockLocalAllocatorLeaderChangeFlag = false

// SyncMaxTS will check whether MaxTS is the biggest one among all Local TSOs this PD is holding when skipCheck is set,
// and write it into all Local TSO Allocators then if it's indeed the biggest one.
func (s *GrpcServer) SyncMaxTS(_ context.Context, request *pdpb.SyncMaxTSRequest) (*pdpb.SyncMaxTSResponse, error) {
	// TODO: support local tso forward in api service mode in the future.
	if err := s.validateInternalRequest(request.GetHeader(), true); err != nil {
		return nil, err
	}
	tsoAllocatorManager := s.GetTSOAllocatorManager()
	// There is no dc-location found in this server, return err.
	if tsoAllocatorManager.GetClusterDCLocationsNumber() == 0 {
		return &pdpb.SyncMaxTSResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
				"empty cluster dc-location found, checker may not work properly"),
		}, nil
	}
	// Get all Local TSO Allocator leaders
	allocatorLeaders, err := tsoAllocatorManager.GetHoldingLocalAllocatorLeaders()
	if err != nil {
		return &pdpb.SyncMaxTSResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}
	if !request.GetSkipCheck() {
		var maxLocalTS *pdpb.Timestamp
		syncedDCs := make([]string, 0, len(allocatorLeaders))
		for _, allocator := range allocatorLeaders {
			// No longer leader, just skip here because
			// the global allocator will check if all DCs are handled.
			if !allocator.IsAllocatorLeader() {
				continue
			}
			currentLocalTSO, err := allocator.GetCurrentTSO()
			if err != nil {
				return &pdpb.SyncMaxTSResponse{
					Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
				}, nil
			}
			if tsoutil.CompareTimestamp(currentLocalTSO, maxLocalTS) > 0 {
				maxLocalTS = currentLocalTSO
			}
			syncedDCs = append(syncedDCs, allocator.GetDCLocation())
		}

		failpoint.Inject("mockLocalAllocatorLeaderChange", func() {
			if !mockLocalAllocatorLeaderChangeFlag {
				maxLocalTS = nil
				request.MaxTs = nil
				mockLocalAllocatorLeaderChangeFlag = true
			}
		})

		if maxLocalTS == nil {
			return &pdpb.SyncMaxTSResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
					"local tso allocator leaders have changed during the sync, should retry"),
			}, nil
		}
		if request.GetMaxTs() == nil {
			return &pdpb.SyncMaxTSResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
					"empty maxTS in the request, should retry"),
			}, nil
		}
		// Found a bigger or equal maxLocalTS, return it directly.
		cmpResult := tsoutil.CompareTimestamp(maxLocalTS, request.GetMaxTs())
		if cmpResult >= 0 {
			// Found an equal maxLocalTS, plus 1 to logical part before returning it.
			// For example, we have a Global TSO t1 and a Local TSO t2, they have the
			// same physical and logical parts. After being differentiating with suffix,
			// there will be (t1.logical << suffixNum + 0) < (t2.logical << suffixNum + N),
			// where N is bigger than 0, which will cause a Global TSO fallback than the previous Local TSO.
			if cmpResult == 0 {
				maxLocalTS.Logical += 1
			}
			return &pdpb.SyncMaxTSResponse{
				Header:     s.header(),
				MaxLocalTs: maxLocalTS,
				SyncedDcs:  syncedDCs,
			}, nil
		}
	}
	syncedDCs := make([]string, 0, len(allocatorLeaders))
	for _, allocator := range allocatorLeaders {
		if !allocator.IsAllocatorLeader() {
			continue
		}
		if err := allocator.WriteTSO(request.GetMaxTs()); err != nil {
			return &pdpb.SyncMaxTSResponse{
				Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
			}, nil
		}
		syncedDCs = append(syncedDCs, allocator.GetDCLocation())
	}
	return &pdpb.SyncMaxTSResponse{
		Header:    s.header(),
		SyncedDcs: syncedDCs,
	}, nil
}

// SplitRegions split regions by the given split keys
func (s *GrpcServer) SplitRegions(ctx context.Context, request *pdpb.SplitRegionsRequest) (*pdpb.SplitRegionsResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).SplitRegions(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.SplitRegionsResponse), err
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.SplitRegionsResponse{Header: s.notBootstrappedHeader()}, nil
	}
	finishedPercentage, newRegionIDs := rc.GetRegionSplitter().SplitRegions(ctx, request.GetSplitKeys(), int(request.GetRetryLimit()))
	return &pdpb.SplitRegionsResponse{
		Header:             s.header(),
		RegionsId:          newRegionIDs,
		FinishedPercentage: uint64(finishedPercentage),
	}, nil
}

// SplitAndScatterRegions split regions by the given split keys, and scatter regions.
// Only regions which splited successfully will be scattered.
// scatterFinishedPercentage indicates the percentage of successfully splited regions that are scattered.
func (s *GrpcServer) SplitAndScatterRegions(ctx context.Context, request *pdpb.SplitAndScatterRegionsRequest) (*pdpb.SplitAndScatterRegionsResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).SplitAndScatterRegions(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.SplitAndScatterRegionsResponse), err
	}
	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.SplitAndScatterRegionsResponse{Header: s.notBootstrappedHeader()}, nil
	}
	splitFinishedPercentage, newRegionIDs := rc.GetRegionSplitter().SplitRegions(ctx, request.GetSplitKeys(), int(request.GetRetryLimit()))
	scatterFinishedPercentage, err := scatterRegions(rc, newRegionIDs, request.GetGroup(), int(request.GetRetryLimit()), false)
	if err != nil {
		return nil, err
	}
	return &pdpb.SplitAndScatterRegionsResponse{
		Header:                    s.header(),
		RegionsId:                 newRegionIDs,
		SplitFinishedPercentage:   uint64(splitFinishedPercentage),
		ScatterFinishedPercentage: uint64(scatterFinishedPercentage),
	}, nil
}

// scatterRegions add operators to scatter regions and return the processed percentage and error
func scatterRegions(cluster *cluster.RaftCluster, regionsID []uint64, group string, retryLimit int, skipStoreLimit bool) (int, error) {
	opsCount, failures, err := cluster.GetRegionScatterer().ScatterRegionsByID(regionsID, group, retryLimit, skipStoreLimit)
	if err != nil {
		return 0, err
	}
	percentage := 100
	if len(failures) > 0 {
		percentage = 100 - 100*len(failures)/(opsCount+len(failures))
		log.Debug("scatter regions", zap.Errors("failures", func() []error {
			r := make([]error, 0, len(failures))
			for _, err := range failures {
				r = append(r, err)
			}
			return r
		}()))
	}
	return percentage, nil
}

// GetDCLocationInfo gets the dc-location info of the given dc-location from PD leader's TSO allocator manager.
func (s *GrpcServer) GetDCLocationInfo(ctx context.Context, request *pdpb.GetDCLocationInfoRequest) (*pdpb.GetDCLocationInfoResponse, error) {
	// TODO: support local tso forward in api service mode in the future.
	var err error
	if err = s.validateInternalRequest(request.GetHeader(), false); err != nil {
		return nil, err
	}
	if !s.member.IsLeader() {
		return nil, ErrNotLeader
	}
	am := s.GetTSOAllocatorManager()
	info, ok := am.GetDCLocationInfo(request.GetDcLocation())
	if !ok {
		am.ClusterDCLocationChecker()
		return &pdpb.GetDCLocationInfoResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
				fmt.Sprintf("dc-location %s is not found", request.GetDcLocation())),
		}, nil
	}
	resp := &pdpb.GetDCLocationInfoResponse{
		Header: s.header(),
		Suffix: info.Suffix,
	}
	// Because the number of suffix bits is changing dynamically according to the dc-location number,
	// there is a corner case may cause the Local TSO is not unique while member changing.
	// Example:
	//     t1: xxxxxxxxxxxxxxx1 | 11
	//     t2: xxxxxxxxxxxxxxx | 111
	// So we will force the newly added Local TSO Allocator to have a Global TSO synchronization
	// when it becomes the Local TSO Allocator leader.
	// Please take a look at https://github.com/tikv/pd/issues/3260 for more details.
	if resp.MaxTs, err = am.GetMaxLocalTSO(ctx); err != nil {
		return &pdpb.GetDCLocationInfoResponse{
			Header: s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN, err.Error()),
		}, nil
	}
	return resp, nil
}

// validateInternalRequest checks if server is closed, which is used to validate
// the gRPC communication between PD servers internally.
func (s *GrpcServer) validateInternalRequest(header *pdpb.RequestHeader, onlyAllowLeader bool) error {
	if s.IsClosed() {
		return ErrNotStarted
	}
	// If onlyAllowLeader is true, check whether the sender is PD leader.
	if onlyAllowLeader {
		leaderID := s.GetLeader().GetMemberId()
		if leaderID != header.GetSenderId() {
			return status.Errorf(codes.FailedPrecondition, "%s, need %d but got %d", errs.MismatchLeaderErr, leaderID, header.GetSenderId())
		}
	}
	return nil
}

func (s *GrpcServer) getDelegateClient(ctx context.Context, forwardedHost string) (*grpc.ClientConn, error) {
	client, ok := s.clientConns.Load(forwardedHost)
	if ok {
		// Mostly, the connection is already established, and return it directly.
		return client.(*grpc.ClientConn), nil
	}

	tlsConfig, err := s.GetTLSConfig().ToTLSConfig()
	if err != nil {
		return nil, err
	}
	ctxTimeout, cancel := context.WithTimeout(ctx, defaultGRPCDialTimeout)
	defer cancel()
	newConn, err := grpcutil.GetClientConn(ctxTimeout, forwardedHost, tlsConfig)
	if err != nil {
		return nil, err
	}
	conn, loaded := s.clientConns.LoadOrStore(forwardedHost, newConn)
	if !loaded {
		// Successfully stored the connection we created.
		return newConn, nil
	}
	// Loaded a connection created/stored by another goroutine, so close the one we created
	// and return the one we loaded.
	newConn.Close()
	return conn.(*grpc.ClientConn), nil
}

func (s *GrpcServer) isLocalRequest(forwardedHost string) bool {
	failpoint.Inject("useForwardRequest", func() {
		failpoint.Return(false)
	})
	if forwardedHost == "" {
		return true
	}
	memberAddrs := s.GetMember().Member().GetClientUrls()
	for _, addr := range memberAddrs {
		if addr == forwardedHost {
			return true
		}
	}
	return false
}

func (s *GrpcServer) createHeartbeatForwardStream(client *grpc.ClientConn) (pdpb.PD_RegionHeartbeatClient, context.CancelFunc, error) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(s.ctx)
	go grpcutil.CheckStream(ctx, cancel, done)
	forwardStream, err := pdpb.NewPDClient(client).RegionHeartbeat(ctx)
	done <- struct{}{}
	return forwardStream, cancel, err
}

func forwardRegionHeartbeatClientToServer(forwardStream pdpb.PD_RegionHeartbeatClient, server *heartbeatServer, errCh chan error) {
	defer logutil.LogPanic()
	defer close(errCh)
	for {
		resp, err := forwardStream.Recv()
		if err != nil {
			errCh <- errors.WithStack(err)
			return
		}
		if err := server.Send(resp); err != nil {
			errCh <- errors.WithStack(err)
			return
		}
	}
}

func (s *GrpcServer) createSchedulingStream(client *grpc.ClientConn) (schedulingpb.Scheduling_RegionHeartbeatClient, context.CancelFunc, error) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(s.ctx)
	go grpcutil.CheckStream(ctx, cancel, done)
	forwardStream, err := schedulingpb.NewSchedulingClient(client).RegionHeartbeat(ctx)
	done <- struct{}{}
	return forwardStream, cancel, err
}

func forwardSchedulingToServer(forwardStream schedulingpb.Scheduling_RegionHeartbeatClient, server *heartbeatServer, errCh chan error) {
	defer logutil.LogPanic()
	defer close(errCh)
	for {
		resp, err := forwardStream.Recv()
		if err != nil {
			errCh <- errors.WithStack(err)
			return
		}
		response := &pdpb.RegionHeartbeatResponse{
			Header: &pdpb.ResponseHeader{
				ClusterId: resp.GetHeader().GetClusterId(),
				// ignore error here
			},
			ChangePeer:      resp.GetChangePeer(),
			TransferLeader:  resp.GetTransferLeader(),
			RegionId:        resp.GetRegionId(),
			RegionEpoch:     resp.GetRegionEpoch(),
			TargetPeer:      resp.GetTargetPeer(),
			Merge:           resp.GetMerge(),
			SplitRegion:     resp.GetSplitRegion(),
			ChangePeerV2:    resp.GetChangePeerV2(),
			SwitchWitnesses: resp.GetSwitchWitnesses(),
		}

		if err := server.Send(response); err != nil {
			errCh <- errors.WithStack(err)
			return
		}
	}
}

func (s *GrpcServer) createTSOForwardStream(
	ctx context.Context, client *grpc.ClientConn,
) (tsopb.TSO_TsoClient, context.Context, context.CancelFunc, error) {
	done := make(chan struct{})
	forwardCtx, cancelForward := context.WithCancel(ctx)
	go grpcutil.CheckStream(forwardCtx, cancelForward, done)
	forwardStream, err := tsopb.NewTSOClient(client).Tso(forwardCtx)
	done <- struct{}{}
	return forwardStream, forwardCtx, cancelForward, err
}

func (s *GrpcServer) createReportBucketsForwardStream(client *grpc.ClientConn) (pdpb.PD_ReportBucketsClient, context.CancelFunc, error) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(s.ctx)
	go grpcutil.CheckStream(ctx, cancel, done)
	forwardStream, err := pdpb.NewPDClient(client).ReportBuckets(ctx)
	done <- struct{}{}
	return forwardStream, cancel, err
}

func forwardReportBucketClientToServer(forwardStream pdpb.PD_ReportBucketsClient, server *bucketHeartbeatServer, errCh chan error) {
	defer logutil.LogPanic()
	defer close(errCh)
	for {
		resp, err := forwardStream.CloseAndRecv()
		if err != nil {
			errCh <- errors.WithStack(err)
			return
		}
		if err := server.Send(resp); err != nil {
			errCh <- errors.WithStack(err)
			return
		}
	}
}

func (s *GrpcServer) getGlobalTSO(ctx context.Context) (pdpb.Timestamp, error) {
	if !s.IsAPIServiceMode() {
		return s.tsoAllocatorManager.HandleRequest(ctx, tso.GlobalDCLocation, 1)
	}
	request := &tsopb.TsoRequest{
		Header: &tsopb.RequestHeader{
			ClusterId:       s.clusterID,
			KeyspaceId:      utils.DefaultKeyspaceID,
			KeyspaceGroupId: utils.DefaultKeyspaceGroupID,
		},
		Count: 1,
	}
	var (
		forwardedHost string
		forwardStream *streamWrapper
		ts            *tsopb.TsoResponse
		err           error
		ok            bool
	)
	handleStreamError := func(err error) (needRetry bool) {
		if strings.Contains(err.Error(), errs.NotLeaderErr) {
			s.tsoPrimaryWatcher.ForceLoad()
			log.Warn("force to load tso primary address due to error", zap.Error(err), zap.String("tso-addr", forwardedHost))
			return true
		}
		if grpcutil.NeedRebuildConnection(err) {
			s.tsoClientPool.Lock()
			delete(s.tsoClientPool.clients, forwardedHost)
			s.tsoClientPool.Unlock()
			log.Warn("client connection removed due to error", zap.Error(err), zap.String("tso-addr", forwardedHost))
			return true
		}
		return false
	}
	for i := 0; i < maxRetryTimesRequestTSOServer; i++ {
		if i > 0 {
			time.Sleep(retryIntervalRequestTSOServer)
		}
		forwardedHost, ok = s.GetServicePrimaryAddr(ctx, utils.TSOServiceName)
		if !ok || forwardedHost == "" {
			return pdpb.Timestamp{}, ErrNotFoundTSOAddr
		}
		forwardStream, err = s.getTSOForwardStream(forwardedHost)
		if err != nil {
			return pdpb.Timestamp{}, err
		}
		start := time.Now()
		forwardStream.Lock()
		err = forwardStream.Send(request)
		if err != nil {
			if needRetry := handleStreamError(err); needRetry {
				forwardStream.Unlock()
				continue
			}
			log.Error("send request to tso primary server failed", zap.Error(err), zap.String("tso-addr", forwardedHost))
			forwardStream.Unlock()
			return pdpb.Timestamp{}, err
		}
		ts, err = forwardStream.Recv()
		forwardStream.Unlock()
		forwardTsoDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			if needRetry := handleStreamError(err); needRetry {
				continue
			}
			log.Error("receive response from tso primary server failed", zap.Error(err), zap.String("tso-addr", forwardedHost))
			return pdpb.Timestamp{}, err
		}
		return *ts.GetTimestamp(), nil
	}
	log.Error("get global tso from tso primary server failed after retry", zap.Error(err), zap.String("tso-addr", forwardedHost))
	return pdpb.Timestamp{}, err
}

func (s *GrpcServer) getTSOForwardStream(forwardedHost string) (*streamWrapper, error) {
	s.tsoClientPool.RLock()
	forwardStream, ok := s.tsoClientPool.clients[forwardedHost]
	s.tsoClientPool.RUnlock()
	if ok {
		// This is the common case to return here
		return forwardStream, nil
	}

	s.tsoClientPool.Lock()
	defer s.tsoClientPool.Unlock()

	// Double check after entering the critical section
	forwardStream, ok = s.tsoClientPool.clients[forwardedHost]
	if ok {
		return forwardStream, nil
	}

	// Now let's create the client connection and the forward stream
	client, err := s.getDelegateClient(s.ctx, forwardedHost)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(s.ctx)
	go grpcutil.CheckStream(ctx, cancel, done)
	tsoClient, err := tsopb.NewTSOClient(client).Tso(ctx)
	done <- struct{}{}
	if err != nil {
		return nil, err
	}
	forwardStream = &streamWrapper{
		TSO_TsoClient: tsoClient,
	}
	s.tsoClientPool.clients[forwardedHost] = forwardStream
	return forwardStream, nil
}

// for CDC compatibility, we need to initialize config path to `globalConfigPath`
const globalConfigPath = "/global/config/"

// StoreGlobalConfig store global config into etcd by transaction
// Since item value needs to support marshal of different struct types,
// it should be set to `Payload bytes` instead of `Value string`
func (s *GrpcServer) StoreGlobalConfig(_ context.Context, request *pdpb.StoreGlobalConfigRequest) (*pdpb.StoreGlobalConfigResponse, error) {
	if s.client == nil {
		return nil, ErrEtcdNotStarted
	}
	configPath := request.GetConfigPath()
	if configPath == "" {
		configPath = globalConfigPath
	}
	ops := make([]clientv3.Op, len(request.Changes))
	for i, item := range request.Changes {
		name := path.Join(configPath, item.GetName())
		switch item.GetKind() {
		case pdpb.EventType_PUT:
			// For CDC compatibility, we need to check the Value field firstly.
			value := item.GetValue()
			if value == "" {
				value = string(item.GetPayload())
			}
			ops[i] = clientv3.OpPut(name, value)
		case pdpb.EventType_DELETE:
			ops[i] = clientv3.OpDelete(name)
		}
	}
	res, err :=
		kv.NewSlowLogTxn(s.client).Then(ops...).Commit()
	if err != nil {
		return &pdpb.StoreGlobalConfigResponse{}, err
	}
	if !res.Succeeded {
		return &pdpb.StoreGlobalConfigResponse{}, errors.Errorf("failed to execute StoreGlobalConfig transaction")
	}
	return &pdpb.StoreGlobalConfigResponse{}, nil
}

// LoadGlobalConfig support 2 ways to load global config from etcd
// - `Names` iteratively get value from `ConfigPath/Name` but not care about revision
// - `ConfigPath` if `Names` is nil can get all values and revision of current path
func (s *GrpcServer) LoadGlobalConfig(ctx context.Context, request *pdpb.LoadGlobalConfigRequest) (*pdpb.LoadGlobalConfigResponse, error) {
	if s.client == nil {
		return nil, ErrEtcdNotStarted
	}
	configPath := request.GetConfigPath()
	if configPath == "" {
		configPath = globalConfigPath
	}
	// Since item value needs to support marshal of different struct types,
	// it should be set to `Payload bytes` instead of `Value string`.
	if request.Names != nil {
		res := make([]*pdpb.GlobalConfigItem, len(request.Names))
		for i, name := range request.Names {
			r, err := s.client.Get(ctx, path.Join(configPath, name))
			if err != nil {
				res[i] = &pdpb.GlobalConfigItem{Name: name, Error: &pdpb.Error{Type: pdpb.ErrorType_UNKNOWN, Message: err.Error()}}
			} else if len(r.Kvs) == 0 {
				msg := "key " + name + " not found"
				res[i] = &pdpb.GlobalConfigItem{Name: name, Error: &pdpb.Error{Type: pdpb.ErrorType_GLOBAL_CONFIG_NOT_FOUND, Message: msg}}
			} else {
				res[i] = &pdpb.GlobalConfigItem{Name: name, Payload: r.Kvs[0].Value, Kind: pdpb.EventType_PUT}
			}
		}
		return &pdpb.LoadGlobalConfigResponse{Items: res}, nil
	}
	r, err := s.client.Get(ctx, configPath, clientv3.WithPrefix())
	if err != nil {
		return &pdpb.LoadGlobalConfigResponse{}, err
	}
	res := make([]*pdpb.GlobalConfigItem, len(r.Kvs))
	for i, value := range r.Kvs {
		res[i] = &pdpb.GlobalConfigItem{Kind: pdpb.EventType_PUT, Name: string(value.Key), Payload: value.Value}
	}
	return &pdpb.LoadGlobalConfigResponse{Items: res, Revision: r.Header.GetRevision()}, nil
}

// WatchGlobalConfig will retry on recoverable errors forever until reconnected
// by Etcd.Watch() as long as the context has not been canceled or timed out.
// Watch on revision which greater than or equal to the required revision.
func (s *GrpcServer) WatchGlobalConfig(req *pdpb.WatchGlobalConfigRequest, server pdpb.PD_WatchGlobalConfigServer) error {
	if s.client == nil {
		return ErrEtcdNotStarted
	}
	ctx, cancel := context.WithCancel(server.Context())
	defer cancel()
	configPath := req.GetConfigPath()
	if configPath == "" {
		configPath = globalConfigPath
	}
	revision := req.GetRevision()
	// If the revision is compacted, will meet required revision has been compacted error.
	// - If required revision < CompactRevision, we need to reload all configs to avoid losing data.
	// - If required revision >= CompactRevision, just keep watching.
	// Use WithPrevKV() to get the previous key-value pair when get Delete Event.
	watchChan := s.client.Watch(ctx, configPath, clientv3.WithPrefix(), clientv3.WithRev(revision), clientv3.WithPrevKV())
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.Context().Done():
			return nil
		case res := <-watchChan:
			if res.Err() != nil {
				var resp pdpb.WatchGlobalConfigResponse
				if revision < res.CompactRevision {
					resp.Header = s.wrapErrorToHeader(pdpb.ErrorType_DATA_COMPACTED,
						fmt.Sprintf("required watch revision: %d is smaller than current compact/min revision %d.", revision, res.CompactRevision))
				} else {
					resp.Header = s.wrapErrorToHeader(pdpb.ErrorType_UNKNOWN,
						fmt.Sprintf("watch channel meet other error %s.", res.Err().Error()))
				}
				if err := server.Send(&resp); err != nil {
					return err
				}
				// Err() indicates that this WatchResponse holds a channel-closing error.
				return res.Err()
			}
			revision = res.Header.GetRevision()

			cfgs := make([]*pdpb.GlobalConfigItem, 0, len(res.Events))
			for _, e := range res.Events {
				// Since item value needs to support marshal of different struct types,
				// it should be set to `Payload bytes` instead of `Value string`.
				switch e.Type {
				case clientv3.EventTypePut:
					cfgs = append(cfgs, &pdpb.GlobalConfigItem{Name: string(e.Kv.Key), Payload: e.Kv.Value, Kind: pdpb.EventType(e.Type)})
				case clientv3.EventTypeDelete:
					if e.PrevKv != nil {
						cfgs = append(cfgs, &pdpb.GlobalConfigItem{Name: string(e.Kv.Key), Payload: e.PrevKv.Value, Kind: pdpb.EventType(e.Type)})
					} else {
						// Prev-kv is compacted means there must have been a delete event before this event,
						// which means that this is just a duplicated event, so we can just ignore it.
						log.Info("previous key-value pair has been compacted", zap.String("required-key", string(e.Kv.Key)))
					}
				}
			}

			if len(cfgs) > 0 {
				if err := server.Send(&pdpb.WatchGlobalConfigResponse{Changes: cfgs, Revision: res.Header.GetRevision()}); err != nil {
					return err
				}
			}
		}
	}
}

// Evict the leaders when the store is damaged. Damaged regions are emergency errors
// and requires user to manually remove the `evict-leader-scheduler` with pd-ctl
func (s *GrpcServer) handleDamagedStore(stats *pdpb.StoreStats) {
	// TODO: regions have no special process for the time being
	// and need to be removed in the future
	damagedRegions := stats.GetDamagedRegionsId()
	if len(damagedRegions) == 0 {
		return
	}

	for _, regionID := range stats.GetDamagedRegionsId() {
		// Remove peers to make sst recovery physically delete files in TiKV.
		err := s.GetHandler().AddRemovePeerOperator(regionID, stats.GetStoreId())
		if err != nil {
			log.Warn("store damaged but can't add remove peer operator",
				zap.Uint64("region-id", regionID), zap.Uint64("store-id", stats.GetStoreId()),
				zap.String("error", err.Error()))
		} else {
			log.Info("added remove peer operator due to damaged region",
				zap.Uint64("region-id", regionID), zap.Uint64("store-id", stats.GetStoreId()))
		}
	}
}

// ReportMinResolvedTS implements gRPC PDServer.
func (s *GrpcServer) ReportMinResolvedTS(ctx context.Context, request *pdpb.ReportMinResolvedTsRequest) (*pdpb.ReportMinResolvedTsResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).ReportMinResolvedTS(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.ReportMinResolvedTsResponse), nil
	}

	rc := s.GetRaftCluster()
	if rc == nil {
		return &pdpb.ReportMinResolvedTsResponse{Header: s.notBootstrappedHeader()}, nil
	}

	storeID := request.GetStoreId()
	minResolvedTS := request.GetMinResolvedTs()
	if err := rc.SetMinResolvedTS(storeID, minResolvedTS); err != nil {
		return nil, err
	}
	log.Debug("updated min resolved-ts",
		zap.Uint64("store", storeID),
		zap.Uint64("min resolved-ts", minResolvedTS))
	return &pdpb.ReportMinResolvedTsResponse{
		Header: s.header(),
	}, nil
}

// SetExternalTimestamp implements gRPC PDServer.
func (s *GrpcServer) SetExternalTimestamp(ctx context.Context, request *pdpb.SetExternalTimestampRequest) (*pdpb.SetExternalTimestampResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).SetExternalTimestamp(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.SetExternalTimestampResponse), nil
	}

	nowTSO, err := s.getGlobalTSO(ctx)
	if err != nil {
		return nil, err
	}
	globalTS := tsoutil.GenerateTS(&nowTSO)
	externalTS := request.GetTimestamp()
	log.Debug("try to set external timestamp",
		zap.Uint64("external-ts", externalTS), zap.Uint64("global-ts", globalTS))
	if err := s.SetExternalTS(externalTS, globalTS); err != nil {
		return &pdpb.SetExternalTimestampResponse{Header: s.invalidValue(err.Error())}, nil
	}
	return &pdpb.SetExternalTimestampResponse{
		Header: s.header(),
	}, nil
}

// GetExternalTimestamp implements gRPC PDServer.
func (s *GrpcServer) GetExternalTimestamp(ctx context.Context, request *pdpb.GetExternalTimestampRequest) (*pdpb.GetExternalTimestampResponse, error) {
	fn := func(ctx context.Context, client *grpc.ClientConn) (interface{}, error) {
		return pdpb.NewPDClient(client).GetExternalTimestamp(ctx, request)
	}
	if rsp, err := s.unaryMiddleware(ctx, request, fn); err != nil {
		return nil, err
	} else if rsp != nil {
		return rsp.(*pdpb.GetExternalTimestampResponse), nil
	}

	timestamp := s.GetExternalTS()
	return &pdpb.GetExternalTimestampResponse{
		Header:    s.header(),
		Timestamp: timestamp,
	}, nil
}

func currentFunction() string {
	counter, _, _, _ := runtime.Caller(1)
	s := strings.Split(runtime.FuncForPC(counter).Name(), ".")
	return s[len(s)-1]
}
