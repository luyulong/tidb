// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	goctx "golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// RegionRequestSender sends KV/Cop requests to tikv server. It handles network
// errors and some region errors internally.
//
// Typically, a KV/Cop request is bind to a region, all keys that are involved
// in the request should be located in the region.
// The sending process begins with looking for the address of leader store's
// address of the target region from cache, and the request is then sent to the
// destination tikv server over TCP connection.
// If region is updated, can be caused by leader transfer, region split, region
// merge, or region balance, tikv server may not able to process request and
// send back a RegionError.
// RegionRequestSender takes care of errors that does not relevant to region
// range, such as 'I/O timeout', 'NotLeader', and 'ServerIsBusy'. For other
// errors, since region range have changed, the request may need to split, so we
// simply return the error to caller.
type RegionRequestSender struct {
	regionCache *RegionCache
	client      Client
	storeAddr   string
}

// NewRegionRequestSender creates a new sender.
func NewRegionRequestSender(regionCache *RegionCache, client Client) *RegionRequestSender {
	return &RegionRequestSender{
		regionCache: regionCache,
		client:      client,
	}
}

// SendReq sends a request to tikv server.
func (s *RegionRequestSender) SendReq(bo *Backoffer, req *tikvrpc.Request, regionID RegionVerID, timeout time.Duration) (*tikvrpc.Response, error) {
	for {
		ctx, err := s.regionCache.GetRPCContext(bo, regionID)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if ctx == nil {
			// If the region is not found in cache, it must be out
			// of date and already be cleaned up. We can skip the
			// RPC by returning RegionError directly.

			// TODO: Change the returned error to something like "region missing in cache",
			// and handle this error like StaleEpoch, which means to re-split the request and retry.
			return tikvrpc.GenRegionErrorResp(req, &errorpb.Error{StaleEpoch: &errorpb.StaleEpoch{}})
		}

		s.storeAddr = ctx.Addr
		resp, retry, err := s.sendReqToRegion(bo, ctx, req, timeout)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if retry {
			continue
		}

		regionErr, err := resp.GetRegionError()
		if err != nil {
			return nil, errors.Trace(err)
		}
		if regionErr != nil {
			retry, err := s.onRegionError(bo, ctx, regionErr)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if retry {
				continue
			}
		}
		return resp, nil
	}
}

func (s *RegionRequestSender) sendReqToRegion(bo *Backoffer, ctx *RPCContext, req *tikvrpc.Request, timeout time.Duration) (resp *tikvrpc.Response, retry bool, err error) {
	if e := tikvrpc.SetContext(req, ctx.KVCtx); e != nil {
		return nil, false, errors.Trace(e)
	}
	context, cancel := goctx.WithTimeout(bo.ctx, timeout)
	defer cancel()
	resp, err = s.client.SendReq(context, ctx.Addr, req)
	if err != nil {
		if e := s.onSendFail(bo, ctx, err); e != nil {
			return nil, false, errors.Trace(e)
		}
		return nil, true, nil
	}
	return
}

func (s *RegionRequestSender) onSendFail(bo *Backoffer, ctx *RPCContext, err error) error {
	// If it failed because the context is canceled, don't retry on this error.
	if errors.Cause(err) == goctx.Canceled || grpc.Code(err) == codes.Canceled {
		return errors.Trace(err)
	}

	s.regionCache.OnRequestFail(ctx)

	// Retry on request failure when it's not canceled.
	// When a store is not available, the leader of related region should be elected quickly.
	// TODO: the number of retry time should be limited:since region may be unavailable
	// when some unrecoverable disaster happened.
	err = bo.Backoff(boTiKVRPC, errors.Errorf("send tikv request error: %v, ctx: %s, try next peer later", err, ctx.KVCtx))
	return errors.Trace(err)
}

func (s *RegionRequestSender) onRegionError(bo *Backoffer, ctx *RPCContext, regionErr *errorpb.Error) (retry bool, err error) {
	reportRegionError(regionErr)
	if notLeader := regionErr.GetNotLeader(); notLeader != nil {
		// Retry if error is `NotLeader`.
		log.Debugf("tikv reports `NotLeader`: %s, ctx: %s, retry later", notLeader, ctx.KVCtx)
		s.regionCache.UpdateLeader(ctx.Region, notLeader.GetLeader().GetStoreId())
		if notLeader.GetLeader() == nil {
			err = bo.Backoff(boRegionMiss, errors.Errorf("not leader: %v, ctx: %s", notLeader, ctx.KVCtx))
			if err != nil {
				return false, errors.Trace(err)
			}
		}
		return true, nil
	}

	if storeNotMatch := regionErr.GetStoreNotMatch(); storeNotMatch != nil {
		// store not match
		log.Warnf("tikv reports `StoreNotMatch`: %s, ctx: %s, retry later", storeNotMatch, ctx.KVCtx)
		s.regionCache.ClearStoreByID(ctx.GetStoreID())
		return true, nil
	}

	if staleEpoch := regionErr.GetStaleEpoch(); staleEpoch != nil {
		log.Debugf("tikv reports `StaleEpoch`, ctx: %s, retry later", ctx.KVCtx)
		err = s.regionCache.OnRegionStale(ctx, staleEpoch.NewRegions)
		return false, errors.Trace(err)
	}
	if regionErr.GetServerIsBusy() != nil {
		log.Warnf("tikv reports `ServerIsBusy`, reason: %s, ctx: %s, retry later", regionErr.GetServerIsBusy().GetReason(), ctx.KVCtx)
		err = bo.Backoff(boServerBusy, errors.Errorf("server is busy, ctx: %s", ctx.KVCtx))
		if err != nil {
			return false, errors.Trace(err)
		}
		return true, nil
	}
	if regionErr.GetStaleCommand() != nil {
		log.Debugf("tikv reports `StaleCommand`, ctx: %s", ctx.KVCtx)
		return true, nil
	}
	if regionErr.GetRaftEntryTooLarge() != nil {
		log.Warnf("tikv reports `RaftEntryTooLarge`, ctx: %s", ctx.KVCtx)
		return false, errors.New(regionErr.String())
	}
	// For other errors, we only drop cache here.
	// Because caller may need to re-split the request.
	log.Debugf("tikv reports region error: %s, ctx: %s", regionErr, ctx.KVCtx)
	s.regionCache.DropRegion(ctx.Region)
	return false, nil
}
