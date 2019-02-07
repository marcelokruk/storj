// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package bwagreement

import (
	"context"
	"strings"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"
	monkit "gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/pkg/auth"
	"storj.io/storj/pkg/certdb"
	"storj.io/storj/pkg/identity"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/storj"
)

var (
	// Error the default bwagreement errs class
	Error = errs.Class("bwagreement error")
	mon   = monkit.Package()
)

// Config is a configuration struct that is everything you need to start an
// agreement receiver responsibility
type Config struct {
}

//UplinkStat contains information about an uplink's returned bandwidth agreement
type UplinkStat struct {
	NodeID            storj.NodeID
	TotalBytes        int64
	PutActionCount    int
	GetActionCount    int
	TotalTransactions int
}

// DB stores bandwidth agreements.
type DB interface {
	// CreateAgreement adds a new bandwidth agreement.
	CreateAgreement(context.Context, *pb.RenterBandwidthAllocation) error
	// GetTotalsSince returns the sum of each bandwidth type after (exluding) a given date range
	GetTotals(context.Context, time.Time, time.Time) (map[storj.NodeID][]int64, error)
	//GetTotals returns stats about an uplink
	GetUplinkStats(context.Context, time.Time, time.Time) ([]UplinkStat, error)
}

// Server is an implementation of the pb.BandwidthServer interface
type Server struct {
	bwdb     DB
	certdb   certdb.DB
	identity *identity.FullIdentity
	logger   *zap.Logger
}

// NewServer creates instance of Server
func NewServer(db DB, upldb certdb.DB, logger *zap.Logger, fID *identity.FullIdentity) *Server {
	// TODO: reorder arguments, rename logger -> log
	return &Server{bwdb: db, certdb: upldb, logger: logger, identity: fID}
}

// Close closes resources
func (s *Server) Close() error { return nil }

// BandwidthAgreements receives and stores bandwidth agreements from storage nodes
func (s *Server) BandwidthAgreements(ctx context.Context, rba *pb.RenterBandwidthAllocation) (reply *pb.AgreementsSummary, err error) {
	defer mon.Task()(&ctx)(&err)
	s.logger.Debug("Received Agreement...")
	reply = &pb.AgreementsSummary{
		Status: pb.AgreementsSummary_REJECTED,
	}
	pba := rba.PayerAllocation
	//verify message content
	pi, err := identity.PeerIdentityFromContext(ctx)
	if err != nil || rba.StorageNodeId != pi.ID {
		return reply, auth.ErrBadID.New("Storage Node ID: %v vs %v", rba.StorageNodeId, pi.ID)
	}
	//todo:  use whitelist for uplinks?
	if pba.SatelliteId != s.identity.ID {
		return reply, pb.ErrPayer.New("Satellite ID: %v vs %v", pba.SatelliteId, s.identity.ID)
	}
	exp := time.Unix(pba.GetExpirationUnixSec(), 0).UTC()
	if exp.Before(time.Now().UTC()) {
		return reply, pb.ErrPayer.Wrap(auth.ErrExpired.New("%v vs %v", exp, time.Now().UTC()))
	}
	//verify message crypto
	if err := auth.VerifyMessage(rba, s.identity.Leaf.PublicKey); err != nil {
		return reply, pb.ErrRenter.Wrap(err)
	}
	if err := auth.VerifyMessage(&pba, nil); err != nil {
		return reply, pb.ErrPayer.Wrap(err)
	}
	//save and return rersults
	if err = s.bwdb.CreateAgreement(ctx, rba); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") ||
			strings.Contains(err.Error(), "violates unique constraint") {
			return reply, pb.ErrPayer.Wrap(auth.ErrSerial.Wrap(err))
		}
		reply.Status = pb.AgreementsSummary_FAIL
		return reply, pb.ErrPayer.Wrap(err)
	}
	reply.Status = pb.AgreementsSummary_OK
	s.logger.Debug("Stored Agreement...")
	return reply, nil
}
