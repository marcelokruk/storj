// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package psclient

import (
	"bufio"
	"crypto/ecdsa"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/gtank/cryptopasta"
	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"golang.org/x/net/context"

	"storj.io/storj/internal/memory"
	"storj.io/storj/pkg/identity"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/ranger"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/pkg/transport"
)

// ClientError is any error returned by the client
var ClientError = errs.Class("piecestore client error")

// Client is an interface describing the functions for interacting with piecestore nodes
type Client interface {
	Meta(ctx context.Context, id PieceID) (*pb.PieceSummary, error)
	Put(ctx context.Context, id PieceID, data io.Reader, ttl time.Time, ba *pb.PayerBandwidthAllocation, authorization *pb.SignedMessage) error
	Get(ctx context.Context, id PieceID, size int64, ba *pb.PayerBandwidthAllocation, authorization *pb.SignedMessage) (ranger.Ranger, error)
	Delete(ctx context.Context, pieceID PieceID, authorization *pb.SignedMessage) error
	io.Closer
}

// Config describes piecestore client parameters
type Config struct {
	MessageSize    memory.Size `help:"starting bandwidth message size" default:"32KiB"`
	MaxMessageSize memory.Size `help:"maximum bandwidth message size" default:"64KiB"`
}

// resetToDefaults ensures that the config arguments have been assigned
func (config *Config) resetToDefaults() {
	if config.MessageSize == 0 {
		config.MessageSize = 32 * memory.KiB
	}
	if config.MaxMessageSize == 0 {
		config.MaxMessageSize = 64 * memory.KiB
	}
}

// PieceStore -- Struct Info needed for protobuf api calls
type PieceStore struct {
	closeFunc func() error              // function that closes the transport connection
	client    pb.PieceStoreRoutesClient // PieceStore for interacting with Storage Node
	selfID    *identity.FullIdentity    // This client's (an uplink) identity
	remoteID  storj.NodeID              // Storage node being connected to

	config Config
}

// NewPSClient initilizes a piecestore client
func NewPSClient(ctx context.Context, tc transport.Client, n *pb.Node, config Config) (Client, error) {
	n.Type.DPanicOnInvalid("new ps client")

	conn, err := tc.DialNode(ctx, n)
	if err != nil {
		return nil, err
	}

	config.resetToDefaults()

	if config.MessageSize < 0 || config.MessageSize > config.MaxMessageSize {
		return nil, ClientError.New("invalid bandwidth message size: %v", config.MessageSize)
	}
	if config.MaxMessageSize < 0 {
		return nil, ClientError.New("invalid max bandwidth message size: %v", config.MaxMessageSize)
	}

	return &PieceStore{
		closeFunc: conn.Close,
		client:    pb.NewPieceStoreRoutesClient(conn),
		selfID:    tc.Identity(),
		remoteID:  n.Id,

		config: config,
	}, nil
}

// NewCustomRoute creates new PieceStore with custom client interface
func NewCustomRoute(client pb.PieceStoreRoutesClient, target *pb.Node, config Config, selfID *identity.FullIdentity) (*PieceStore, error) {
	target.Type.DPanicOnInvalid("new custom route")

	config.resetToDefaults()

	if config.MessageSize < 0 || config.MessageSize > config.MaxMessageSize {
		return nil, ClientError.New("invalid bandwidth message size: %v", config.MessageSize)
	}
	if config.MaxMessageSize < 0 {
		return nil, ClientError.New("invalid max bandwidth message size: %v", config.MaxMessageSize)
	}

	return &PieceStore{
		client:   client,
		selfID:   selfID,
		remoteID: target.Id,

		config: config,
	}, nil
}

// Close closes the connection with piecestore
func (ps *PieceStore) Close() error {
	if ps.closeFunc == nil {
		return nil
	}

	return ps.closeFunc()
}

// Meta requests info about a piece by Id
func (ps *PieceStore) Meta(ctx context.Context, id PieceID) (*pb.PieceSummary, error) {
	return ps.client.Piece(ctx, &pb.PieceId{Id: id.String()})
}

// Put uploads a Piece to a piece store Server
func (ps *PieceStore) Put(ctx context.Context, id PieceID, data io.Reader, ttl time.Time, ba *pb.PayerBandwidthAllocation, authorization *pb.SignedMessage) error {
	stream, err := ps.client.Store(ctx)
	if err != nil {
		return err
	}

	msg := &pb.PieceStore{
		PieceData:     &pb.PieceStore_PieceData{Id: id.String(), ExpirationUnixSec: ttl.Unix()},
		Authorization: authorization,
	}
	if err = stream.Send(msg); err != nil {
		if _, closeErr := stream.CloseAndRecv(); closeErr != nil {
			zap.S().Errorf("error closing stream %s :: %v.Send() = %v", closeErr, stream, closeErr)
		}

		return fmt.Errorf("%v.Send() = %v", stream, err)
	}

	writer := &StreamWriter{signer: ps, stream: stream, pba: ba}

	defer func() {
		if err := writer.Close(); err != nil && err != io.EOF {
			log.Printf("failed to close writer: %s\n", err)
		}
	}()

	bufw := bufio.NewWriterSize(writer, 32*1024)

	_, err = io.Copy(bufw, data)
	if err == io.ErrUnexpectedEOF {
		_ = writer.Close()
		zap.S().Infof("Node cut from upload due to slow connection. Deleting piece %s...", id)
		deleteErr := ps.Delete(ctx, id, authorization)
		if deleteErr != nil {
			return deleteErr
		}
	}
	if err != nil {
		return err
	}

	return bufw.Flush()
}

// Get begins downloading a Piece from a piece store Server
func (ps *PieceStore) Get(ctx context.Context, id PieceID, size int64, ba *pb.PayerBandwidthAllocation, authorization *pb.SignedMessage) (ranger.Ranger, error) {
	stream, err := ps.client.Retrieve(ctx)
	if err != nil {
		return nil, err
	}

	return PieceRangerSize(ps, stream, id, size, ba, authorization), nil
}

// Delete a Piece from a piece store Server
func (ps *PieceStore) Delete(ctx context.Context, id PieceID, authorization *pb.SignedMessage) error {
	reply, err := ps.client.Delete(ctx, &pb.PieceDelete{Id: id.String(), Authorization: authorization})
	if err != nil {
		return err
	}
	zap.S().Infof("Delete request route summary: %v", reply)
	return nil
}

// sign a message using the clients private key
func (ps *PieceStore) sign(msg []byte) (signature []byte, err error) {
	if ps.selfID == nil || ps.selfID.Key == nil {
		return nil, ClientError.New("failed to sign msg: Private Key not Set")
	}
	return cryptopasta.Sign(msg, ps.selfID.Key.(*ecdsa.PrivateKey))
}

//certs returns this uplink's certificates
func (ps *PieceStore) certs() [][]byte {
	return ps.selfID.ChainRaw()
}
