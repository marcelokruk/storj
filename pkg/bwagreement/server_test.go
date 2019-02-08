// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package bwagreement_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"storj.io/storj/internal/testcontext"
	"storj.io/storj/internal/testidentity"
	"storj.io/storj/pkg/auth"
	"storj.io/storj/pkg/bwagreement"
	"storj.io/storj/pkg/bwagreement/testbwagreement"
	"storj.io/storj/pkg/identity"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/satellitedb/satellitedbtest"
)

func TestBandwidthAgreement(t *testing.T) {
	satellitedbtest.Run(t, func(t *testing.T, db satellite.DB) {
		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		testDatabase(ctx, t, db)
	})
}

func getPeerContext(ctx context.Context, t *testing.T) (context.Context, storj.NodeID) {
	ident, err := testidentity.NewTestIdentity(ctx)
	if !assert.NoError(t, err) || !assert.NotNil(t, ident) {
		t.Fatal(err)
	}
	grpcPeer := &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 5},
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{ident.Leaf, ident.CA},
			},
		},
	}
	nodeID, err := identity.NodeIDFromKey(ident.CA.PublicKey)
	assert.NoError(t, err)
	return peer.NewContext(ctx, grpcPeer), nodeID
}

func testDatabase(ctx context.Context, t *testing.T, db satellite.DB) {
	upID, err := testidentity.NewTestIdentity(ctx)
	assert.NoError(t, err)
	satID, err := testidentity.NewTestIdentity(ctx)
	assert.NoError(t, err)
	satellite := bwagreement.NewServer(db.BandwidthAgreement(), db.CertDB(), zap.NewNop(), satID)
	err = db.CertDB().SavePublicKey(ctx, upID.ID, upID.Leaf.PublicKey)
	assert.NoError(t, err)

	{ // TestSameSerialNumberBandwidthAgreements
		pbaFile1, err := testbwagreement.GeneratePayerBandwidthAllocation(pb.BandwidthAction_GET, satID, upID, time.Hour)
		assert.NoError(t, err)

		ctxSN1, storageNode1 := getPeerContext(ctx, t)
		rbaNode1, err := testbwagreement.GenerateRenterBandwidthAllocation(pbaFile1, storageNode1, upID, 666)
		assert.NoError(t, err)

		ctxSN2, storageNode2 := getPeerContext(ctx, t)
		rbaNode2, err := testbwagreement.GenerateRenterBandwidthAllocation(pbaFile1, storageNode2, upID, 666)
		assert.NoError(t, err)

		/* More than one storage node can submit bwagreements with the same serial number.
		   Uplink would like to download a file from 2 storage nodes.
		   Uplink requests a PayerBandwidthAllocation from the satellite. One serial number for all storage nodes.
		   Uplink signes 2 RenterBandwidthAllocation for both storage node. */
		{
			reply, err := satellite.BandwidthAgreements(ctxSN1, rbaNode1)
			assert.NoError(t, err)
			assert.Equal(t, pb.AgreementsSummary_OK, reply.Status)

			reply, err = satellite.BandwidthAgreements(ctxSN2, rbaNode2)
			assert.NoError(t, err)
			assert.Equal(t, pb.AgreementsSummary_OK, reply.Status)
		}

		/* Storage node can submit a second bwagreement with a different sequence value.
		   Uplink downloads another file. New PayerBandwidthAllocation with a new sequence. */
		{
			pbaFile2, err := testbwagreement.GeneratePayerBandwidthAllocation(pb.BandwidthAction_GET, satID, upID, time.Hour)
			assert.NoError(t, err)

			rbaNode1, err := testbwagreement.GenerateRenterBandwidthAllocation(pbaFile2, storageNode1, upID, 666)
			assert.NoError(t, err)

			reply, err := satellite.BandwidthAgreements(ctxSN1, rbaNode1)
			assert.NoError(t, err)
			assert.Equal(t, pb.AgreementsSummary_OK, reply.Status)
		}

		/* Storage nodes can't submit a second bwagreement with the same sequence. */
		{
			rbaNode1, err := testbwagreement.GenerateRenterBandwidthAllocation(pbaFile1, storageNode1, upID, 666)
			assert.NoError(t, err)

			reply, err := satellite.BandwidthAgreements(ctxSN1, rbaNode1)
			assert.True(t, auth.ErrSerial.Has(err), err.Error())
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}

		/* Storage nodes can't submit the same bwagreement twice.
		   This test is kind of duplicate cause it will most likely trigger the same sequence error.
		   For safety we will try it anyway to make sure nothing strange will happen */
		{
			reply, err := satellite.BandwidthAgreements(ctxSN2, rbaNode2)
			assert.True(t, auth.ErrSerial.Has(err))
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}
	}

	{ // TestExpiredBandwidthAgreements
		{ // storage nodes can submit a bwagreement that will expire in 30 seconds
			pba, err := testbwagreement.GeneratePayerBandwidthAllocation(pb.BandwidthAction_GET, satID, upID, 30*time.Second)
			assert.NoError(t, err)

			ctxSN1, storageNode1 := getPeerContext(ctx, t)
			rba, err := testbwagreement.GenerateRenterBandwidthAllocation(pba, storageNode1, upID, 666)
			assert.NoError(t, err)

			reply, err := satellite.BandwidthAgreements(ctxSN1, rba)
			assert.NoError(t, err)
			assert.Equal(t, pb.AgreementsSummary_OK, reply.Status)
		}

		{ // storage nodes can't submit a bwagreement that expires right now
			pba, err := testbwagreement.GeneratePayerBandwidthAllocation(pb.BandwidthAction_GET, satID, upID, 0*time.Second)
			assert.NoError(t, err)

			ctxSN1, storageNode1 := getPeerContext(ctx, t)
			rba, err := testbwagreement.GenerateRenterBandwidthAllocation(pba, storageNode1, upID, 666)
			assert.NoError(t, err)

			reply, err := satellite.BandwidthAgreements(ctxSN1, rba)
			assert.Error(t, err)
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}

		{ // storage nodes can't submit a bwagreement that expires yesterday
			pba, err := testbwagreement.GeneratePayerBandwidthAllocation(pb.BandwidthAction_GET, satID, upID, -23*time.Hour-55*time.Second)
			assert.NoError(t, err)

			ctxSN1, storageNode1 := getPeerContext(ctx, t)
			rba, err := testbwagreement.GenerateRenterBandwidthAllocation(pba, storageNode1, upID, 666)
			assert.NoError(t, err)

			reply, err := satellite.BandwidthAgreements(ctxSN1, rba)
			assert.Error(t, err)
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}
	}

	{ // TestManipulatedBandwidthAgreements
		pba, err := testbwagreement.GeneratePayerBandwidthAllocation(pb.BandwidthAction_GET, satID, upID, time.Hour)
		if !assert.NoError(t, err) {
			t.Fatal(err)
		}

		ctxSN1, storageNode1 := getPeerContext(ctx, t)
		rba, err := testbwagreement.GenerateRenterBandwidthAllocation(pba, storageNode1, upID, 666)
		assert.NoError(t, err)

		// Storage node manipulates the bwagreement
		rba.Total = 1337

		// Generate a new keypair for self signing bwagreements
		manipID, err := testidentity.NewTestIdentity(ctx)
		assert.NoError(t, err)

		/* Storage node can't manipulate the bwagreement size (or any other field)
		   Satellite will verify Renter's Signature. */
		{
			manipRBA := *rba
			// Using uplink signatur
			reply, err := satellite.BandwidthAgreements(ctxSN1, &manipRBA)
			assert.True(t, auth.ErrVerify.Has(err) && pb.ErrRenter.Has(err), err.Error())
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}

		/* Storage node can't sign the manipulated bwagreement
		   Satellite will verify Renter's Signature. */
		{
			manipRBA := *rba
			assert.NoError(t, auth.SignMessage(&manipRBA, manipID.Key))
			// Using self created signature
			reply, err := satellite.BandwidthAgreements(ctxSN1, &manipRBA)
			assert.True(t, auth.ErrVerify.Has(err) && pb.ErrRenter.Has(err), err.Error())
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}

		/* Storage node can't replace uplink NodeId
		   Satellite will verify the Payer's Signature. */
		{
			manipRBA := *rba
			// Overwrite the uplinkId with our own keypair
			manipRBA.PayerAllocation.UplinkId = manipID.ID
			assert.NoError(t, auth.SignMessage(&manipRBA, manipID.Key))
			// Using self created signature + public key
			reply, err := satellite.BandwidthAgreements(ctxSN1, &manipRBA)
			assert.True(t, auth.ErrVerify.Has(err) && pb.ErrRenter.Has(err), err.Error())
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}

		/* Storage node can't self sign the PayerBandwidthAllocation.
		   Satellite will verify the Payer's Signature. */
		{
			manipRBA := *rba
			// Overwrite the uplinkId with our own keypair
			manipRBA.PayerAllocation.UplinkId = manipID.ID
			assert.NoError(t, auth.SignMessage(&manipRBA.PayerAllocation, manipID.Key))
			assert.NoError(t, auth.SignMessage(&manipRBA, manipID.Key))
			// Using self created Payer and Renter bwagreement signatures
			reply, err := satellite.BandwidthAgreements(ctxSN1, &manipRBA)
			assert.True(t, auth.ErrVerify.Has(err) && pb.ErrRenter.Has(err), err.Error())
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}

		/* Storage node can't replace the satellite.
		   Satellite will verify the Satellite Id. */
		{
			manipRBA := *rba
			// Overwrite the uplinkId and satelliteID with our own keypair
			manipRBA.PayerAllocation.UplinkId = manipID.ID
			manipRBA.PayerAllocation.SatelliteId = manipID.ID
			assert.NoError(t, auth.SignMessage(&manipRBA.PayerAllocation, manipID.Key))
			assert.NoError(t, auth.SignMessage(&manipRBA, manipID.Key))
			// Using self created Payer and Renter bwagreement signatures
			reply, err := satellite.BandwidthAgreements(ctxSN1, &manipRBA)
			assert.True(t, pb.ErrPayer.Has(err), err.Error())
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}
	}

	{ //TestInvalidBandwidthAgreements
		ctxSN1, storageNode1 := getPeerContext(ctx, t)
		pba, err := testbwagreement.GeneratePayerBandwidthAllocation(pb.BandwidthAction_GET, satID, upID, time.Hour)
		assert.NoError(t, err)

		{ // Storage node sends an corrupted signuature to force a satellite crash
			rba, err := testbwagreement.GenerateRenterBandwidthAllocation(pba, storageNode1, upID, 666)
			assert.NoError(t, err)
			rba.Signature = []byte("invalid")
			reply, err := satellite.BandwidthAgreements(ctxSN1, rba)
			assert.Error(t, err)
			assert.True(t, pb.ErrRenter.Has(err), err.Error())
			assert.Equal(t, pb.AgreementsSummary_REJECTED, reply.Status)
		}
	}
}

func callBWA(ctx context.Context, t *testing.T, sat *bwagreement.Server, signature []byte, rba *pb.RenterBandwidthAllocation) (*pb.AgreementsSummary, error) {
	rba.SetSignature(signature)
	return sat.BandwidthAgreements(ctx, rba)
}
