// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package metainfo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/common/context2"
	"storj.io/common/encryption"
	"storj.io/common/macaroon"
	"storj.io/common/memory"
	"storj.io/common/pb"
	"storj.io/common/rpc/rpcstatus"
	"storj.io/common/signing"
	"storj.io/common/storj"
	"storj.io/common/uuid"
	lrucache "storj.io/storj/pkg/cache"
	"storj.io/storj/satellite/accounting"
	"storj.io/storj/satellite/attribution"
	"storj.io/storj/satellite/console"
	"storj.io/storj/satellite/internalpb"
	"storj.io/storj/satellite/metainfo/metabase"
	"storj.io/storj/satellite/metainfo/piecedeletion"
	"storj.io/storj/satellite/metainfo/pointerverification"
	"storj.io/storj/satellite/orders"
	"storj.io/storj/satellite/overlay"
	"storj.io/storj/satellite/revocation"
	"storj.io/storj/satellite/rewards"
	"storj.io/uplink/private/eestream"
)

const (
	satIDExpiration = 48 * time.Hour

	deleteObjectPiecesSuccessThreshold = 0.75
)

var (
	mon = monkit.Package()
	// Error general metainfo error.
	Error = errs.Class("metainfo error")
	// ErrNodeAlreadyExists pointer already has a piece for a node err.
	ErrNodeAlreadyExists = errs.Class("metainfo error: node already exists")
)

// APIKeys is api keys store methods used by endpoint.
//
// architecture: Database
type APIKeys interface {
	GetByHead(ctx context.Context, head []byte) (*console.APIKeyInfo, error)
}

// Endpoint metainfo endpoint.
//
// architecture: Endpoint
type Endpoint struct {
	log                  *zap.Logger
	metainfo             *Service
	deletePieces         *piecedeletion.Service
	orders               *orders.Service
	overlay              *overlay.Service
	attributions         attribution.DB
	partners             *rewards.PartnersService
	pointerVerification  *pointerverification.Service
	projectUsage         *accounting.Service
	projects             console.Projects
	apiKeys              APIKeys
	satellite            signing.Signer
	limiterCache         *lrucache.ExpiringLRU
	encInlineSegmentSize int64 // max inline segment size + encryption overhead
	revocations          revocation.DB
	defaultRS            *pb.RedundancyScheme
	config               Config
}

// NewEndpoint creates new metainfo endpoint instance.
func NewEndpoint(log *zap.Logger, metainfo *Service, deletePieces *piecedeletion.Service,
	orders *orders.Service, cache *overlay.Service, attributions attribution.DB,
	partners *rewards.PartnersService, peerIdentities overlay.PeerIdentities,
	apiKeys APIKeys, projectUsage *accounting.Service, projects console.Projects,
	satellite signing.Signer, revocations revocation.DB, config Config) (*Endpoint, error) {
	// TODO do something with too many params

	encInlineSegmentSize, err := encryption.CalcEncryptedSize(config.MaxInlineSegmentSize.Int64(), storj.EncryptionParameters{
		CipherSuite: storj.EncAESGCM,
		BlockSize:   128, // intentionally low block size to allow maximum possible encryption overhead
	})
	if err != nil {
		return nil, err
	}

	defaultRSScheme := &pb.RedundancyScheme{
		Type:             pb.RedundancyScheme_RS,
		MinReq:           int32(config.RS.Min),
		RepairThreshold:  int32(config.RS.Repair),
		SuccessThreshold: int32(config.RS.Success),
		Total:            int32(config.RS.Total),
		ErasureShareSize: config.RS.ErasureShareSize.Int32(),
	}

	return &Endpoint{
		log:                 log,
		metainfo:            metainfo,
		deletePieces:        deletePieces,
		orders:              orders,
		overlay:             cache,
		attributions:        attributions,
		partners:            partners,
		pointerVerification: pointerverification.NewService(peerIdentities),
		apiKeys:             apiKeys,
		projectUsage:        projectUsage,
		projects:            projects,
		satellite:           satellite,
		limiterCache: lrucache.New(lrucache.Options{
			Capacity:   config.RateLimiter.CacheCapacity,
			Expiration: config.RateLimiter.CacheExpiration,
		}),
		encInlineSegmentSize: encInlineSegmentSize,
		revocations:          revocations,
		defaultRS:            defaultRSScheme,
		config:               config,
	}, nil
}

// Close closes resources.
func (endpoint *Endpoint) Close() error { return nil }

func calculateSpaceUsed(segmentSize int64, numberOfPieces int, rs storj.RedundancyScheme) (totalStored int64) {
	pieceSize := segmentSize / int64(rs.RequiredShares)
	return pieceSize * int64(numberOfPieces)
}

// filterValidPieces filter out the invalid remote pieces held by pointer.
//
// This method expect the pointer to be valid, so it has to be validated before
// calling it.
//
// The method always return a gRPC status error so the caller can directly
// return it to the client.
// func (endpoint *Endpoint) filterValidPieces(ctx context.Context, pointer *pb.Pointer, originalLimits []*pb.OrderLimit) (err error) {
// 	defer mon.Task()(&ctx)(&err)

// 	if pointer.Type != pb.Pointer_REMOTE {
// 		return nil
// 	}

// 	// verify that the piece sizes matches what we would expect.
// 	err = endpoint.pointerVerification.VerifySizes(ctx, pointer)
// 	if err != nil {
// 		endpoint.log.Debug("piece sizes are invalid", zap.Error(err))
// 		return rpcstatus.Errorf(rpcstatus.InvalidArgument, "piece sizes are invalid: %v", err)
// 	}

// 	validPieces, invalidPieces, err := endpoint.pointerVerification.SelectValidPieces(ctx, pointer, originalLimits)
// 	if err != nil {
// 		endpoint.log.Debug("pointer verification failed", zap.Error(err))
// 		return rpcstatus.Errorf(rpcstatus.InvalidArgument, "pointer verification failed: %s", err)
// 	}

// 	remote := pointer.Remote

// 	if int32(len(validPieces)) < remote.Redundancy.SuccessThreshold {
// 		endpoint.log.Debug("Number of valid pieces is less than the success threshold",
// 			zap.Int("totalReceivedPieces", len(remote.RemotePieces)),
// 			zap.Int("validPieces", len(validPieces)),
// 			zap.Int("invalidPieces", len(invalidPieces)),
// 			zap.Int32("successThreshold", remote.Redundancy.SuccessThreshold),
// 		)

// 		errMsg := fmt.Sprintf("Number of valid pieces (%d) is less than the success threshold (%d). Found %d invalid pieces",
// 			len(validPieces),
// 			remote.Redundancy.SuccessThreshold,
// 			len(remote.RemotePieces),
// 		)
// 		if len(invalidPieces) > 0 {
// 			errMsg = fmt.Sprintf("%s. Invalid Pieces:", errMsg)

// 			for _, p := range invalidPieces {
// 				errMsg = fmt.Sprintf("%s\nNodeID: %v, PieceNum: %d, Reason: %s",
// 					errMsg, p.NodeID, p.PieceNum, p.Reason,
// 				)
// 			}
// 		}

// 		return rpcstatus.Error(rpcstatus.InvalidArgument, errMsg)
// 	}

// 	remote.RemotePieces = validPieces

// 	return nil
// }

// ProjectInfo returns allowed ProjectInfo for the provided API key.
func (endpoint *Endpoint) ProjectInfo(ctx context.Context, req *pb.ProjectInfoRequest) (_ *pb.ProjectInfoResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:   macaroon.ActionProjectInfo,
		Time: time.Now(),
	})
	if err != nil {
		return nil, err
	}

	salt := sha256.Sum256(keyInfo.ProjectID[:])

	return &pb.ProjectInfoResponse{
		ProjectSalt: salt[:],
	}, nil
}

// GetBucket returns a bucket.
func (endpoint *Endpoint) GetBucket(ctx context.Context, req *pb.BucketGetRequest) (resp *pb.BucketGetResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:     macaroon.ActionRead,
		Bucket: req.Name,
		Time:   time.Now(),
	})
	if err != nil {
		return nil, err
	}

	bucket, err := endpoint.metainfo.GetBucket(ctx, req.GetName(), keyInfo.ProjectID)
	if err != nil {
		if storj.ErrBucketNotFound.Has(err) {
			return nil, rpcstatus.Error(rpcstatus.NotFound, err.Error())
		}
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	// override RS to fit satellite settings
	convBucket, err := convertBucketToProto(bucket, endpoint.defaultRS)
	if err != nil {
		return resp, err
	}

	return &pb.BucketGetResponse{
		Bucket: convBucket,
	}, nil
}

// CreateBucket creates a new bucket.
func (endpoint *Endpoint) CreateBucket(ctx context.Context, req *pb.BucketCreateRequest) (resp *pb.BucketCreateResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:     macaroon.ActionWrite,
		Bucket: req.Name,
		Time:   time.Now(),
	})
	if err != nil {
		return nil, err
	}

	err = endpoint.validateBucket(ctx, req.Name)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	// checks if bucket exists before updates it or makes a new entry
	_, err = endpoint.metainfo.GetBucket(ctx, req.GetName(), keyInfo.ProjectID)
	if err == nil {
		// When the bucket exists, try to set the attribution.
		if err := endpoint.ensureAttribution(ctx, req.Header, keyInfo, req.GetName()); err != nil {
			return nil, err
		}
		return nil, rpcstatus.Error(rpcstatus.AlreadyExists, "bucket already exists")
	}
	if !storj.ErrBucketNotFound.Has(err) {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	// check if project has exceeded its allocated bucket limit
	maxBuckets, err := endpoint.projects.GetMaxBuckets(ctx, keyInfo.ProjectID)
	if err != nil {
		return nil, err
	}
	if maxBuckets == nil {
		defaultMaxBuckets := endpoint.config.ProjectLimits.MaxBuckets
		maxBuckets = &defaultMaxBuckets
	}
	bucketCount, err := endpoint.metainfo.CountBuckets(ctx, keyInfo.ProjectID)
	if err != nil {
		return nil, err
	}
	if bucketCount >= *maxBuckets {
		return nil, rpcstatus.Error(rpcstatus.ResourceExhausted, fmt.Sprintf("number of allocated buckets (%d) exceeded", endpoint.config.ProjectLimits.MaxBuckets))
	}

	bucketReq, err := convertProtoToBucket(req, keyInfo.ProjectID)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	bucket, err := endpoint.metainfo.CreateBucket(ctx, bucketReq)
	if err != nil {
		endpoint.log.Error("error while creating bucket", zap.String("bucketName", bucketReq.Name), zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, "unable to create bucket")
	}

	// Once we have created the bucket, we can try setting the attribution.
	if err := endpoint.ensureAttribution(ctx, req.Header, keyInfo, req.GetName()); err != nil {
		return nil, err
	}

	// override RS to fit satellite settings
	convBucket, err := convertBucketToProto(bucket, endpoint.defaultRS)
	if err != nil {
		endpoint.log.Error("error while converting bucket to proto", zap.String("bucketName", bucket.Name), zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, "unable to create bucket")
	}

	return &pb.BucketCreateResponse{
		Bucket: convBucket,
	}, nil
}

// DeleteBucket deletes a bucket.
func (endpoint *Endpoint) DeleteBucket(ctx context.Context, req *pb.BucketDeleteRequest) (resp *pb.BucketDeleteResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	now := time.Now()

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:     macaroon.ActionDelete,
		Bucket: req.Name,
		Time:   now,
	})
	if err != nil {
		return nil, err
	}

	err = endpoint.validateBucket(ctx, req.Name)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	_, err = endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:     macaroon.ActionRead,
		Bucket: req.Name,
		Time:   now,
	})
	canRead := err == nil

	_, err = endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:     macaroon.ActionList,
		Bucket: req.Name,
		Time:   now,
	})
	canList := err == nil

	var (
		bucket     storj.Bucket
		convBucket *pb.Bucket
	)
	if canRead || canList {
		// Info about deleted bucket is returned only if either Read, or List permission is granted.
		bucket, err = endpoint.metainfo.GetBucket(ctx, req.Name, keyInfo.ProjectID)
		if err != nil {
			if storj.ErrBucketNotFound.Has(err) {
				return nil, rpcstatus.Error(rpcstatus.NotFound, err.Error())
			}
			return nil, err
		}

		convBucket, err = convertBucketToProto(bucket, endpoint.defaultRS)
		if err != nil {
			return nil, err
		}
	}

	err = endpoint.metainfo.DeleteBucket(ctx, req.Name, keyInfo.ProjectID)
	if err != nil {
		if !canRead && !canList {
			// No error info is returned if neither Read, nor List permission is granted.
			return &pb.BucketDeleteResponse{}, nil
		}
		if ErrBucketNotEmpty.Has(err) {
			// List permission is required to delete all objects in a bucket.
			if !req.GetDeleteAll() || !canList {
				return nil, rpcstatus.Error(rpcstatus.FailedPrecondition, err.Error())
			}

			_, deletedObjCount, err := endpoint.deleteBucketNotEmpty(ctx, keyInfo.ProjectID, req.Name)
			if err != nil {
				return nil, err
			}

			return &pb.BucketDeleteResponse{Bucket: convBucket, DeletedObjectsCount: deletedObjCount}, nil
		}
		if storj.ErrBucketNotFound.Has(err) {
			return &pb.BucketDeleteResponse{Bucket: convBucket}, nil
		}
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	return &pb.BucketDeleteResponse{Bucket: convBucket}, nil
}

// deleteBucketNotEmpty deletes all objects from bucket and deletes this bucket.
// On success, it returns only the number of deleted objects.
func (endpoint *Endpoint) deleteBucketNotEmpty(ctx context.Context, projectID uuid.UUID, bucketName []byte) ([]byte, int64, error) {
	deletedCount, err := endpoint.deleteBucketObjects(ctx, projectID, bucketName)
	if err != nil {
		return nil, 0, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	err = endpoint.metainfo.DeleteBucket(ctx, bucketName, projectID)
	if err != nil {
		if ErrBucketNotEmpty.Has(err) {
			return nil, deletedCount, rpcstatus.Error(rpcstatus.FailedPrecondition, "cannot delete the bucket because it's being used by another process")
		}
		if storj.ErrBucketNotFound.Has(err) {
			return bucketName, 0, nil
		}
		return nil, deletedCount, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	return bucketName, deletedCount, nil
}

// deleteBucketObjects deletes all objects in a bucket.
func (endpoint *Endpoint) deleteBucketObjects(ctx context.Context, projectID uuid.UUID, bucketName []byte) (_ int64, err error) {
	defer mon.Task()(&ctx)(&err)

	bucketLocation := metabase.BucketLocation{ProjectID: projectID, BucketName: string(bucketName)}
	deletedObjects, err := endpoint.metainfo.metabaseDB.DeleteBucketObjects(ctx, metabase.DeleteBucketObjects{
		Bucket: bucketLocation,
		DeletePieces: func(ctx context.Context, deleted []metabase.DeletedSegmentInfo) error {
			endpoint.deleteSegmentPieces(ctx, deleted)
			return nil
		},
	})

	return deletedObjects, Error.Wrap(err)
}

// ListBuckets returns buckets in a project where the bucket name matches the request cursor.
func (endpoint *Endpoint) ListBuckets(ctx context.Context, req *pb.BucketListRequest) (resp *pb.BucketListResponse, err error) {
	defer mon.Task()(&ctx)(&err)
	action := macaroon.Action{
		// TODO: This has to be ActionList, but it seems to be set to
		// ActionRead as a hacky workaround to make bucket listing possible.
		Op:   macaroon.ActionRead,
		Time: time.Now(),
	}
	keyInfo, err := endpoint.validateAuth(ctx, req.Header, action)
	if err != nil {
		return nil, err
	}

	allowedBuckets, err := getAllowedBuckets(ctx, req.Header, action)
	if err != nil {
		return nil, err
	}

	listOpts := storj.BucketListOptions{
		Cursor:    string(req.Cursor),
		Limit:     int(req.Limit),
		Direction: storj.ListDirection(req.Direction),
	}
	bucketList, err := endpoint.metainfo.ListBuckets(ctx, keyInfo.ProjectID, listOpts, allowedBuckets)
	if err != nil {
		return nil, err
	}

	bucketItems := make([]*pb.BucketListItem, len(bucketList.Items))
	for i, item := range bucketList.Items {
		bucketItems[i] = &pb.BucketListItem{
			Name:      []byte(item.Name),
			CreatedAt: item.Created,
		}
	}

	return &pb.BucketListResponse{
		Items: bucketItems,
		More:  bucketList.More,
	}, nil
}

// CountBuckets returns the number of buckets a project currently has.
// TODO: add this to the uplink client side.
func (endpoint *Endpoint) CountBuckets(ctx context.Context, projectID uuid.UUID) (count int, err error) {
	count, err = endpoint.metainfo.CountBuckets(ctx, projectID)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func getAllowedBuckets(ctx context.Context, header *pb.RequestHeader, action macaroon.Action) (_ macaroon.AllowedBuckets, err error) {
	key, err := getAPIKey(ctx, header)
	if err != nil {
		return macaroon.AllowedBuckets{}, rpcstatus.Errorf(rpcstatus.InvalidArgument, "Invalid API credentials: %v", err)
	}
	allowedBuckets, err := key.GetAllowedBuckets(ctx, action)
	if err != nil {
		return macaroon.AllowedBuckets{}, rpcstatus.Errorf(rpcstatus.Internal, "GetAllowedBuckets: %v", err)
	}
	return allowedBuckets, err
}

func convertProtoToBucket(req *pb.BucketCreateRequest, projectID uuid.UUID) (bucket storj.Bucket, err error) {
	bucketID, err := uuid.New()
	if err != nil {
		return storj.Bucket{}, err
	}

	defaultRS := req.GetDefaultRedundancyScheme()
	defaultEP := req.GetDefaultEncryptionParameters()

	// TODO: resolve partner id
	var partnerID uuid.UUID
	err = partnerID.UnmarshalJSON(req.GetPartnerId())

	// bucket's partnerID should never be set
	// it is always read back from buckets DB
	if err != nil && !partnerID.IsZero() {
		return bucket, errs.New("Invalid uuid")
	}

	return storj.Bucket{
		ID:                  bucketID,
		Name:                string(req.GetName()),
		ProjectID:           projectID,
		PartnerID:           partnerID,
		PathCipher:          storj.CipherSuite(req.GetPathCipher()),
		DefaultSegmentsSize: req.GetDefaultSegmentSize(),
		DefaultRedundancyScheme: storj.RedundancyScheme{
			Algorithm:      storj.RedundancyAlgorithm(defaultRS.GetType()),
			ShareSize:      defaultRS.GetErasureShareSize(),
			RequiredShares: int16(defaultRS.GetMinReq()),
			RepairShares:   int16(defaultRS.GetRepairThreshold()),
			OptimalShares:  int16(defaultRS.GetSuccessThreshold()),
			TotalShares:    int16(defaultRS.GetTotal()),
		},
		DefaultEncryptionParameters: storj.EncryptionParameters{
			CipherSuite: storj.CipherSuite(defaultEP.CipherSuite),
			BlockSize:   int32(defaultEP.BlockSize),
		},
	}, nil
}

func convertBucketToProto(bucket storj.Bucket, rs *pb.RedundancyScheme) (pbBucket *pb.Bucket, err error) {
	if bucket == (storj.Bucket{}) {
		return nil, nil
	}

	partnerID, err := bucket.PartnerID.MarshalJSON()
	if err != nil {
		return pbBucket, rpcstatus.Error(rpcstatus.Internal, "UUID marshal error")
	}

	pbBucket = &pb.Bucket{
		Name:                    []byte(bucket.Name),
		PathCipher:              pb.CipherSuite(bucket.PathCipher),
		PartnerId:               partnerID,
		CreatedAt:               bucket.Created,
		DefaultSegmentSize:      bucket.DefaultSegmentsSize,
		DefaultRedundancyScheme: rs,
		DefaultEncryptionParameters: &pb.EncryptionParameters{
			CipherSuite: pb.CipherSuite(bucket.DefaultEncryptionParameters.CipherSuite),
			BlockSize:   int64(bucket.DefaultEncryptionParameters.BlockSize),
		},
	}

	// this part is to provide default ciphers (path and encryption) for old uplinks
	// new uplinks are using ciphers from encryption access
	if pbBucket.PathCipher == pb.CipherSuite_ENC_UNSPECIFIED {
		pbBucket.PathCipher = pb.CipherSuite_ENC_AESGCM
	}
	if pbBucket.DefaultEncryptionParameters.CipherSuite == pb.CipherSuite_ENC_UNSPECIFIED {
		pbBucket.DefaultEncryptionParameters.CipherSuite = pb.CipherSuite_ENC_AESGCM
		pbBucket.DefaultEncryptionParameters.BlockSize = int64(rs.ErasureShareSize * rs.MinReq)
	}

	return pbBucket, nil
}

// BeginObject begins object.
func (endpoint *Endpoint) BeginObject(ctx context.Context, req *pb.ObjectBeginRequest) (resp *pb.ObjectBeginResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionWrite,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	if !req.ExpiresAt.IsZero() && !req.ExpiresAt.After(time.Now()) {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, "Invalid expiration time")
	}

	err = endpoint.validateBucket(ctx, req.Bucket)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	// TODO this needs to be optimized to avoid DB call on each request
	_, err = endpoint.metainfo.GetBucket(ctx, req.Bucket, keyInfo.ProjectID)
	if err != nil {
		if storj.ErrBucketNotFound.Has(err) {
			return nil, rpcstatus.Error(rpcstatus.NotFound, err.Error())
		}

		endpoint.log.Error("unable to check bucket", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	_, err = endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionDelete,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPath,
		Time:          time.Now(),
	})
	canDelete := err == nil

	if canDelete {
		_, err = endpoint.DeleteCommittedObject(ctx, keyInfo.ProjectID, string(req.Bucket), metabase.ObjectKey(req.EncryptedPath))
		if err != nil {
			return nil, err
		}
	} else {
		_, err = endpoint.metainfo.metabaseDB.GetObjectLatestVersion(ctx, metabase.GetObjectLatestVersion{
			ObjectLocation: metabase.ObjectLocation{
				ProjectID:  keyInfo.ProjectID,
				BucketName: string(req.Bucket),
				ObjectKey:  metabase.ObjectKey(req.EncryptedPath),
			},
		})
		if err == nil {
			return nil, rpcstatus.Error(rpcstatus.PermissionDenied, "Unauthorized API credentials")
		}
	}

	if err := endpoint.ensureAttribution(ctx, req.Header, keyInfo, req.Bucket); err != nil {
		return nil, err
	}

	// use only satellite values for Redundancy Scheme
	pbRS := endpoint.defaultRS
	streamID, err := uuid.New()
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	// TODO this will work only with newsest uplink
	// figue out what to do with this
	encryptionParameters := storj.EncryptionParameters{
		CipherSuite: storj.CipherSuite(req.EncryptionParameters.CipherSuite),
		BlockSize:   int32(req.EncryptionParameters.BlockSize), // TODO check conversion
	}

	var expiresAt *time.Time
	if req.ExpiresAt.IsZero() {
		expiresAt = nil
	} else {
		expiresAt = &req.ExpiresAt
	}

	_, err = endpoint.metainfo.metabaseDB.BeginObjectExactVersion(ctx, metabase.BeginObjectExactVersion{
		ObjectStream: metabase.ObjectStream{
			ProjectID:  keyInfo.ProjectID,
			BucketName: string(req.Bucket),
			ObjectKey:  metabase.ObjectKey(req.EncryptedPath),
			StreamID:   streamID,
			Version:    metabase.Version(1),
		},
		ExpiresAt:  expiresAt,
		Encryption: encryptionParameters,
	})
	if err != nil {
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	satStreamID, err := endpoint.packStreamID(ctx, &internalpb.StreamID{
		Bucket:         req.Bucket,
		EncryptedPath:  req.EncryptedPath,
		Version:        req.Version,
		Redundancy:     pbRS,
		CreationDate:   time.Now(),
		ExpirationDate: req.ExpiresAt,
		StreamId:       streamID[:],
	})
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	endpoint.log.Info("Object Upload", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "put"), zap.String("type", "object"))
	mon.Meter("req_put_object").Mark(1)

	return &pb.ObjectBeginResponse{
		Bucket:           req.Bucket,
		EncryptedPath:    req.EncryptedPath,
		Version:          req.Version,
		StreamId:         satStreamID,
		RedundancyScheme: pbRS,
	}, nil
}

// CommitObject commits an object when all its segments have already been committed.
func (endpoint *Endpoint) CommitObject(ctx context.Context, req *pb.ObjectCommitRequest) (resp *pb.ObjectCommitResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	return endpoint.commitObject(ctx, req, nil)
}

func (endpoint *Endpoint) commitObject(ctx context.Context, req *pb.ObjectCommitRequest, pointer *pb.Pointer) (resp *pb.ObjectCommitResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	streamID, err := endpoint.unmarshalSatStreamID(ctx, req.StreamId)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionWrite,
		Bucket:        streamID.Bucket,
		EncryptedPath: streamID.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	metadataSize := memory.Size(len(req.EncryptedMetadata))
	if metadataSize > endpoint.config.MaxMetadataSize {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, fmt.Sprintf("Metadata is too large, got %v, maximum allowed is %v", metadataSize, endpoint.config.MaxMetadataSize))
	}

	id, err := uuid.FromBytes(streamID.StreamId)
	if err != nil {
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	_, err = endpoint.metainfo.metabaseDB.CommitObject(ctx, metabase.CommitObject{
		ObjectStream: metabase.ObjectStream{
			ProjectID:  keyInfo.ProjectID,
			BucketName: string(streamID.Bucket),
			ObjectKey:  metabase.ObjectKey(streamID.EncryptedPath),
			StreamID:   id,
			Version:    metabase.Version(1),
		},
		EncryptedMetadata:             req.EncryptedMetadata,
		EncryptedMetadataNonce:        req.EncryptedMetadataNonce[:],
		EncryptedMetadataEncryptedKey: req.EncryptedMetadataEncryptedKey,
	})
	if err != nil {
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	return &pb.ObjectCommitResponse{}, nil
}

// GetObject gets single object.
func (endpoint *Endpoint) GetObject(ctx context.Context, req *pb.ObjectGetRequest) (resp *pb.ObjectGetResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionRead,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	err = endpoint.validateBucket(ctx, req.Bucket)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	object, err := endpoint.getObject(ctx, keyInfo.ProjectID, req.Bucket, req.EncryptedPath, req.Version)
	if err != nil {
		return nil, err
	}

	endpoint.log.Info("Object Download", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "get"), zap.String("type", "object"))
	mon.Meter("req_get_object").Mark(1)

	return &pb.ObjectGetResponse{
		Object: object,
	}, nil
}

func (endpoint *Endpoint) getObject(ctx context.Context, projectID uuid.UUID, bucket, encryptedPath []byte, version int32) (*pb.Object, error) {
	metaObject, err := endpoint.metainfo.metabaseDB.GetObjectLatestVersion(ctx, metabase.GetObjectLatestVersion{
		ObjectLocation: metabase.ObjectLocation{
			ProjectID:  projectID,
			BucketName: string(bucket),
			ObjectKey:  metabase.ObjectKey(encryptedPath),
		},
	})
	if err != nil {
		if storj.ErrObjectNotFound.Has(err) {
			return nil, rpcstatus.Error(rpcstatus.NotFound, err.Error())
		}
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	object, err := endpoint.objectToProto(ctx, metaObject)
	if err != nil {
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	return object, nil
}

// ListObjects list objects according to specific parameters.
func (endpoint *Endpoint) ListObjects(ctx context.Context, req *pb.ObjectListRequest) (resp *pb.ObjectListResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionList,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPrefix,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	err = endpoint.validateBucket(ctx, req.Bucket)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	// TODO this needs to be optimized to avoid DB call on each request
	_, err = endpoint.metainfo.GetBucket(ctx, req.Bucket, keyInfo.ProjectID)
	if err != nil {
		if storj.ErrBucketNotFound.Has(err) {
			return nil, rpcstatus.Error(rpcstatus.NotFound, err.Error())
		}
		endpoint.log.Error("unable to check bucket", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	limit := int(req.Limit)
	if limit < 0 {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, "limit is negative")
	}
	if limit == 0 {
		limit = metabase.MaxListLimit
	}

	var prefix metabase.ObjectKey
	if len(req.EncryptedPrefix) != 0 {
		prefix = metabase.ObjectKey(req.EncryptedPrefix)
		if prefix[len(prefix)-1] != metabase.Delimiter {
			prefix += metabase.ObjectKey(metabase.Delimiter)
		}
	}

	// Default to Commmitted status for backward-compatibility with older uplinks.
	status := metabase.Committed
	if req.Status != pb.Object_INVALID {
		status = metabase.ObjectStatus(req.Status)
	}

	cursor := string(req.EncryptedCursor)
	if len(cursor) != 0 {
		cursor = string(prefix) + cursor
	}

	resp = &pb.ObjectListResponse{}
	// TODO: Replace with IterateObjectsLatestVersion when ready
	err = endpoint.metainfo.metabaseDB.IterateObjectsAllVersionsWithStatus(ctx,
		metabase.IterateObjectsWithStatus{
			ProjectID:  keyInfo.ProjectID,
			BucketName: string(req.Bucket),
			Prefix:     prefix,
			Cursor: metabase.IterateCursor{
				Key:     metabase.ObjectKey(cursor),
				Version: 1, // TODO: set to a the version from the protobuf request when it supports this
			},
			Recursive: req.Recursive,
			BatchSize: limit + 1,
			Status:    status,
		}, func(ctx context.Context, it metabase.ObjectsIterator) error {
			entry := metabase.ObjectEntry{}
			for len(resp.Items) < limit && it.Next(ctx, &entry) {
				item, err := endpoint.objectEntryToProtoListItem(ctx, req.Bucket, entry)
				if err != nil {
					return err
				}
				resp.Items = append(resp.Items, item)
			}
			resp.More = it.Next(ctx, &entry)
			return nil
		},
	)
	if err != nil {
		if metabase.ErrInvalidRequest.Has(err) {
			return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
		}
		endpoint.log.Error("unable to list objects", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	endpoint.log.Info("Object List", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "list"), zap.String("type", "object"))
	mon.Meter("req_list_object").Mark(1)

	return resp, nil
}

// BeginDeleteObject begins object deletion process.
func (endpoint *Endpoint) BeginDeleteObject(ctx context.Context, req *pb.ObjectBeginDeleteRequest) (resp *pb.ObjectBeginDeleteResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	now := time.Now()

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionDelete,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPath,
		Time:          now,
	})
	if err != nil {
		return nil, err
	}

	err = endpoint.validateBucket(ctx, req.Bucket)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	_, err = endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionRead,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPath,
		Time:          now,
	})
	canRead := err == nil

	_, err = endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionList,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPath,
		Time:          now,
	})
	canList := err == nil

	var deletedObjects []*pb.Object

	if req.GetStatus() == int32(metabase.Pending) {
		if req.StreamId == nil {
			return nil, rpcstatus.Error(rpcstatus.InvalidArgument, "StreamID missing")
		}
		var pbStreamID *internalpb.StreamID
		pbStreamID, err = endpoint.unmarshalSatStreamID(ctx, *(req.StreamId))
		if err == nil {
			var streamID uuid.UUID
			streamID, err = uuid.FromBytes(pbStreamID.StreamId)
			if err == nil {
				deletedObjects, err = endpoint.DeletePendingObject(ctx, keyInfo.ProjectID, string(req.Bucket), metabase.ObjectKey(req.EncryptedPath), req.GetVersion(), streamID)
			}
		}
	} else {
		deletedObjects, err = endpoint.DeleteCommittedObject(ctx, keyInfo.ProjectID, string(req.Bucket), metabase.ObjectKey(req.EncryptedPath))
	}
	if err != nil {
		if !canRead && !canList {
			// No error info is returned if neither Read, nor List permission is granted
			return &pb.ObjectBeginDeleteResponse{}, nil
		}
		return nil, err
	}

	var object *pb.Object
	if canRead || canList {
		// Info about deleted object is returned only if either Read, or List permission is granted
		if err != nil {
			endpoint.log.Error("failed to construct deleted object information",
				zap.Stringer("Project ID", keyInfo.ProjectID),
				zap.String("Bucket", string(req.Bucket)),
				zap.String("Encrypted Path", string(req.EncryptedPath)),
				zap.Error(err),
			)
		}
		if len(deletedObjects) > 0 {
			object = deletedObjects[0]
		}
	}

	endpoint.log.Info("Object Delete", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "delete"), zap.String("type", "object"))
	mon.Meter("req_delete_object").Mark(1)

	return &pb.ObjectBeginDeleteResponse{
		Object: object,
	}, nil
}

// FinishDeleteObject finishes object deletion.
func (endpoint *Endpoint) FinishDeleteObject(ctx context.Context, req *pb.ObjectFinishDeleteRequest) (resp *pb.ObjectFinishDeleteResponse, err error) {
	// all logic for deleting is now in BeginDeleteObject
	return nil, rpcstatus.Error(rpcstatus.Unimplemented, "not implemented")
}

// GetObjectIPs returns the IP addresses of the nodes holding the pieces for
// the provided object. This is useful for knowing the locations of the pieces.
func (endpoint *Endpoint) GetObjectIPs(ctx context.Context, req *pb.ObjectGetIPsRequest) (resp *pb.ObjectGetIPsResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionRead,
		Bucket:        req.Bucket,
		EncryptedPath: req.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	err = endpoint.validateBucket(ctx, req.Bucket)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	// TODO we may need custom metabase request to avoid two DB calls
	object, err := endpoint.metainfo.metabaseDB.GetObjectLatestVersion(ctx, metabase.GetObjectLatestVersion{
		ObjectLocation: metabase.ObjectLocation{
			ProjectID:  keyInfo.ProjectID,
			BucketName: string(req.Bucket),
			ObjectKey:  metabase.ObjectKey(req.EncryptedPath),
		},
	})
	if err != nil {
		if storj.ErrObjectNotFound.Has(err) {
			return nil, rpcstatus.Error(rpcstatus.NotFound, err.Error())
		}
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	more := true
	cursor := metabase.SegmentPosition{}

	var nodeIDs []storj.NodeID
	for more {
		list, err := endpoint.metainfo.metabaseDB.ListSegments(ctx, metabase.ListSegments{
			StreamID: object.StreamID,
			Cursor:   cursor,
		})
		if err != nil {
			endpoint.log.Error("internal", zap.Error(err))
			return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
		}

		for _, segment := range list.Segments {
			for _, piece := range segment.Pieces {
				nodeIDs = append(nodeIDs, piece.StorageNode)
			}
			cursor = segment.Position
		}
		more = list.More
	}

	nodes, err := endpoint.overlay.GetOnlineNodesForGetDelete(ctx, nodeIDs)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	resp = &pb.ObjectGetIPsResponse{}
	for _, node := range nodes {
		address := node.Address.GetAddress()
		if address != "" {
			resp.Ips = append(resp.Ips, []byte(address))
		}
	}

	return resp, nil
}

// BeginSegment begins segment uploading.
func (endpoint *Endpoint) BeginSegment(ctx context.Context, req *pb.SegmentBeginRequest) (resp *pb.SegmentBeginResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	streamID, err := endpoint.unmarshalSatStreamID(ctx, req.StreamId)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionWrite,
		Bucket:        streamID.Bucket,
		EncryptedPath: streamID.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	// no need to validate streamID fields because it was validated during BeginObject

	if req.Position.Index < 0 {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, "segment index must be greater then 0")
	}

	exceeded, limit, err := endpoint.projectUsage.ExceedsStorageUsage(ctx, keyInfo.ProjectID)
	if err != nil {
		endpoint.log.Error("Retrieving project storage totals failed.", zap.Error(err))
	}
	if exceeded {
		endpoint.log.Error("Monthly storage limit exceeded.",
			zap.Stringer("Limit", limit),
			zap.Stringer("Project ID", keyInfo.ProjectID),
		)
		return nil, rpcstatus.Error(rpcstatus.ResourceExhausted, "Exceeded Usage Limit")
	}

	redundancy, err := eestream.NewRedundancyStrategyFromProto(streamID.Redundancy)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	maxPieceSize := eestream.CalcPieceSize(req.MaxOrderLimit, redundancy)

	request := overlay.FindStorageNodesRequest{
		RequestedCount: redundancy.TotalCount(),
	}
	nodes, err := endpoint.overlay.FindStorageNodesForUpload(ctx, request)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	bucket := metabase.BucketLocation{ProjectID: keyInfo.ProjectID, BucketName: string(streamID.Bucket)}
	rootPieceID, addressedLimits, piecePrivateKey, err := endpoint.orders.CreatePutOrderLimits(ctx, bucket, nodes, streamID.ExpirationDate, maxPieceSize)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	id, err := uuid.FromBytes(streamID.StreamId)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	pieces := metabase.Pieces{}
	for i, limit := range addressedLimits {
		pieces = append(pieces, metabase.Piece{
			Number:      uint16(i),
			StorageNode: limit.Limit.StorageNodeId,
		})
	}
	err = endpoint.metainfo.metabaseDB.BeginSegment(ctx, metabase.BeginSegment{
		ObjectStream: metabase.ObjectStream{
			ProjectID:  keyInfo.ProjectID,
			BucketName: string(streamID.Bucket),
			ObjectKey:  metabase.ObjectKey(streamID.EncryptedPath),
			StreamID:   id,
			Version:    1,
		},
		Position: metabase.SegmentPosition{
			Part:  uint32(req.Position.PartNumber),
			Index: uint32(req.Position.Index),
		},
		RootPieceID: rootPieceID,
		Pieces:      pieces,
	})
	if err != nil {
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	segmentID, err := endpoint.packSegmentID(ctx, &internalpb.SegmentID{
		StreamId:            streamID,
		PartNumber:          req.Position.PartNumber,
		Index:               req.Position.Index,
		OriginalOrderLimits: addressedLimits,
		RootPieceId:         rootPieceID,
		CreationDate:        time.Now(),
	})

	endpoint.log.Info("Segment Upload", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "put"), zap.String("type", "remote"))
	mon.Meter("req_put_remote").Mark(1)

	return &pb.SegmentBeginResponse{
		SegmentId:        segmentID,
		AddressedLimits:  addressedLimits,
		PrivateKey:       piecePrivateKey,
		RedundancyScheme: endpoint.defaultRS,
	}, nil
}

// CommitSegment commits segment after uploading.
func (endpoint *Endpoint) CommitSegment(ctx context.Context, req *pb.SegmentCommitRequest) (resp *pb.SegmentCommitResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	_, resp, err = endpoint.commitSegment(ctx, req, true)
	return resp, err
}

func (endpoint *Endpoint) commitSegment(ctx context.Context, req *pb.SegmentCommitRequest, savePointer bool) (_ *pb.Pointer, resp *pb.SegmentCommitResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	segmentID, err := endpoint.unmarshalSatSegmentID(ctx, req.SegmentId)
	if err != nil {
		return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	streamID := segmentID.StreamId

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionWrite,
		Bucket:        streamID.Bucket,
		EncryptedPath: streamID.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, nil, err
	}

	if numResults := len(req.UploadResult); numResults < int(streamID.Redundancy.GetSuccessThreshold()) {
		endpoint.log.Debug("the results of uploaded pieces for the segment is below the redundancy optimal threshold",
			zap.Int("upload pieces results", numResults),
			zap.Int32("redundancy optimal threshold", streamID.Redundancy.GetSuccessThreshold()),
			zap.Stringer("Segment ID", req.SegmentId),
		)
		return nil, nil, rpcstatus.Errorf(rpcstatus.InvalidArgument,
			"the number of results of uploaded pieces (%d) is below the optimal threshold (%d)",
			numResults, streamID.Redundancy.GetSuccessThreshold(),
		)
	}

	// TODO bring back validation

	// orderLimits := make([]*pb.OrderLimit, len(segmentID.OriginalOrderLimits))
	// for i, orderLimit := range segmentID.OriginalOrderLimits {
	// 	orderLimits[i] = orderLimit.Limit
	// }

	// err = endpoint.validatePointer(ctx, pointer, orderLimits)
	// if err != nil {
	// 	return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	// }

	// err = endpoint.filterValidPieces(ctx, pointer, orderLimits)
	// if err != nil {
	// 	return nil, nil, err
	// }

	exceeded, limit, err := endpoint.projectUsage.ExceedsStorageUsage(ctx, keyInfo.ProjectID)
	if err != nil {
		return nil, nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}
	if exceeded {
		endpoint.log.Error("The project limit of storage and bandwidth has been exceeded",
			zap.Int64("limit", limit.Int64()),
			zap.Stringer("Project ID", keyInfo.ProjectID),
		)
		return nil, nil, rpcstatus.Error(rpcstatus.ResourceExhausted, "Exceeded Usage Limit")
	}

	pieces := metabase.Pieces{}
	for _, result := range req.UploadResult {
		pieces = append(pieces, metabase.Piece{
			Number:      uint16(result.PieceNum),
			StorageNode: result.NodeId,
		})
	}

	rs := storj.RedundancyScheme{
		Algorithm:      storj.RedundancyAlgorithm(endpoint.defaultRS.Type),
		RequiredShares: int16(endpoint.defaultRS.MinReq),
		RepairShares:   int16(endpoint.defaultRS.RepairThreshold),
		OptimalShares:  int16(endpoint.defaultRS.SuccessThreshold),
		TotalShares:    int16(endpoint.defaultRS.Total),
		ShareSize:      endpoint.defaultRS.ErasureShareSize,
	}

	segmentSize := req.SizeEncryptedData
	totalStored := calculateSpaceUsed(segmentSize, len(pieces), rs)

	// ToDo: Replace with hash & signature validation
	// Ensure neither uplink or storage nodes are cheating on us

	// We cannot have more redundancy than total/min
	if float64(totalStored) > (float64(segmentSize)/float64(rs.RequiredShares))*float64(rs.TotalShares) {
		endpoint.log.Debug("data size mismatch",
			zap.Int64("segment", segmentSize),
			zap.Int64("pieces", totalStored),
			zap.Int16("redundancy minimum requested", rs.RequiredShares),
			zap.Int16("redundancy total", rs.TotalShares),
		)
		return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, "mismatched segment size and piece usage")
	}

	if err := endpoint.projectUsage.AddProjectStorageUsage(ctx, keyInfo.ProjectID, segmentSize); err != nil {
		endpoint.log.Error("Could not track new storage usage by project",
			zap.Stringer("Project ID", keyInfo.ProjectID),
			zap.Error(err),
		)
		// but continue. it's most likely our own fault that we couldn't track it, and the only thing
		// that will be affected is our per-project bandwidth and storage limits.
	}

	id, err := uuid.FromBytes(streamID.StreamId)
	if err != nil {
		return nil, nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	err = endpoint.metainfo.metabaseDB.CommitSegment(ctx, metabase.CommitSegment{
		ObjectStream: metabase.ObjectStream{
			ProjectID:  keyInfo.ProjectID,
			BucketName: string(streamID.Bucket),
			ObjectKey:  metabase.ObjectKey(streamID.EncryptedPath),
			StreamID:   id,
			Version:    1,
		},
		EncryptedKey:      req.EncryptedKey,
		EncryptedKeyNonce: req.EncryptedKeyNonce[:],

		EncryptedSize: int32(req.SizeEncryptedData), // TODO incompatible types int32 vs int64
		PlainSize:     int32(req.PlainSize),         // TODO incompatible types int32 vs int64

		Position: metabase.SegmentPosition{
			Part:  uint32(segmentID.PartNumber),
			Index: uint32(segmentID.Index),
		},
		RootPieceID: segmentID.RootPieceId,
		Redundancy:  rs,
		Pieces:      pieces,
	})
	if err != nil {
		if metabase.ErrInvalidRequest.Has(err) {
			return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
		}
		endpoint.log.Error("internal", zap.Error(err))
		return nil, nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	return nil, &pb.SegmentCommitResponse{
		SuccessfulPieces: int32(len(pieces)),
	}, nil
}

// MakeInlineSegment makes inline segment on satellite.
func (endpoint *Endpoint) MakeInlineSegment(ctx context.Context, req *pb.SegmentMakeInlineRequest) (resp *pb.SegmentMakeInlineResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	_, resp, err = endpoint.makeInlineSegment(ctx, req, true)
	return resp, err
}

// makeInlineSegment makes inline segment on satellite.
func (endpoint *Endpoint) makeInlineSegment(ctx context.Context, req *pb.SegmentMakeInlineRequest, savePointer bool) (pointer *pb.Pointer, resp *pb.SegmentMakeInlineResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	streamID, err := endpoint.unmarshalSatStreamID(ctx, req.StreamId)
	if err != nil {
		return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionWrite,
		Bucket:        streamID.Bucket,
		EncryptedPath: streamID.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, nil, err
	}

	if req.Position.Index < 0 {
		return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, "segment index must be greater then 0")
	}

	inlineUsed := int64(len(req.EncryptedInlineData))
	// TODO silently ignore empty inline segmens to be backward compatible with old uplinks.
	// Metabase is rejecting empty inline segments.
	if inlineUsed == 0 {
		return nil, &pb.SegmentMakeInlineResponse{}, nil
	}
	if inlineUsed > endpoint.encInlineSegmentSize {
		return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, fmt.Sprintf("inline segment size cannot be larger than %s", endpoint.config.MaxInlineSegmentSize))
	}

	exceeded, limit, err := endpoint.projectUsage.ExceedsStorageUsage(ctx, keyInfo.ProjectID)
	if err != nil {
		return nil, nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}
	if exceeded {
		endpoint.log.Error("Monthly storage limit exceeded.",
			zap.Stringer("Limit", limit),
			zap.Stringer("Project ID", keyInfo.ProjectID),
		)
		return nil, nil, rpcstatus.Error(rpcstatus.ResourceExhausted, "Exceeded Usage Limit")
	}

	if err := endpoint.projectUsage.AddProjectStorageUsage(ctx, keyInfo.ProjectID, inlineUsed); err != nil {
		endpoint.log.Error("Could not track new storage usage.", zap.Stringer("Project ID", keyInfo.ProjectID), zap.Error(err))
		// but continue. it's most likely our own fault that we couldn't track it, and the only thing
		// that will be affected is our per-project bandwidth and storage limits.
	}

	id, err := uuid.FromBytes(streamID.StreamId)
	if err != nil {
		return nil, nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	err = endpoint.metainfo.metabaseDB.CommitInlineSegment(ctx, metabase.CommitInlineSegment{
		ObjectStream: metabase.ObjectStream{
			ProjectID:  keyInfo.ProjectID,
			BucketName: string(streamID.Bucket),
			ObjectKey:  metabase.ObjectKey(streamID.EncryptedPath),
			StreamID:   id,
			Version:    1,
		},
		EncryptedKey:      req.EncryptedKey,
		EncryptedKeyNonce: req.EncryptedKeyNonce.Bytes(),

		Position: metabase.SegmentPosition{
			Part:  uint32(req.Position.PartNumber),
			Index: uint32(req.Position.Index),
		},

		PlainSize: int32(req.PlainSize), // TODO incompatible types int32 vs int64

		InlineData: req.EncryptedInlineData,
	})
	if err != nil {
		if metabase.ErrInvalidRequest.Has(err) {
			return nil, nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
		}
		endpoint.log.Error("internal", zap.Error(err))
		return nil, nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	bucket := metabase.BucketLocation{ProjectID: keyInfo.ProjectID, BucketName: string(streamID.Bucket)}
	err = endpoint.orders.UpdatePutInlineOrder(ctx, bucket, inlineUsed)
	if err != nil {
		return nil, nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	endpoint.log.Info("Inline Segment Upload", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "put"), zap.String("type", "inline"))
	mon.Meter("req_put_inline").Mark(1)

	return nil, &pb.SegmentMakeInlineResponse{}, nil
}

// BeginDeleteSegment begins segment deletion process.
func (endpoint *Endpoint) BeginDeleteSegment(ctx context.Context, req *pb.SegmentBeginDeleteRequest) (resp *pb.SegmentBeginDeleteResponse, err error) {
	// all logic for deleting is now in BeginDeleteObject
	return nil, rpcstatus.Error(rpcstatus.Unimplemented, "not implemented")
}

// FinishDeleteSegment finishes segment deletion process.
func (endpoint *Endpoint) FinishDeleteSegment(ctx context.Context, req *pb.SegmentFinishDeleteRequest) (resp *pb.SegmentFinishDeleteResponse, err error) {
	// all logic for deleting is now in BeginDeleteObject
	return nil, rpcstatus.Error(rpcstatus.Unimplemented, "not implemented")
}

// ListSegments list object segments.
func (endpoint *Endpoint) ListSegments(ctx context.Context, req *pb.SegmentListRequest) (resp *pb.SegmentListResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	streamID, err := endpoint.unmarshalSatStreamID(ctx, req.StreamId)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	_, err = endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionRead,
		Bucket:        streamID.Bucket,
		EncryptedPath: streamID.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	cursor := req.CursorPosition
	if cursor == nil {
		cursor = &pb.SegmentPosition{}
	}

	id, err := uuid.FromBytes(streamID.StreamId)
	if err != nil {
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	result, err := endpoint.metainfo.metabaseDB.ListSegments(ctx, metabase.ListSegments{
		StreamID: id,
		Cursor: metabase.SegmentPosition{
			Part:  uint32(cursor.PartNumber),
			Index: uint32(cursor.Index),
		},
		Limit: int(req.Limit),
	})
	if err != nil {
		if storj.ErrObjectNotFound.Has(err) {
			return nil, rpcstatus.Error(rpcstatus.NotFound, err.Error())
		}
		endpoint.log.Error("internal", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	items := make([]*pb.SegmentListItem, len(result.Segments))
	for i, item := range result.Segments {
		items[i] = &pb.SegmentListItem{
			Position: &pb.SegmentPosition{
				PartNumber: int32(item.Position.Part),
				Index:      int32(item.Position.Index),
			},
			PlainSize: int64(item.PlainSize),
		}
	}

	return &pb.SegmentListResponse{
		Items: items,
		More:  result.More,
	}, nil
}

// DownloadSegment returns data necessary to download segment.
func (endpoint *Endpoint) DownloadSegment(ctx context.Context, req *pb.SegmentDownloadRequest) (resp *pb.SegmentDownloadResponse, err error) {
	defer mon.Task()(&ctx)(&err)

	streamID, err := endpoint.unmarshalSatStreamID(ctx, req.StreamId)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, err.Error())
	}

	keyInfo, err := endpoint.validateAuth(ctx, req.Header, macaroon.Action{
		Op:            macaroon.ActionRead,
		Bucket:        streamID.Bucket,
		EncryptedPath: streamID.EncryptedPath,
		Time:          time.Now(),
	})
	if err != nil {
		return nil, err
	}

	bucket := metabase.BucketLocation{ProjectID: keyInfo.ProjectID, BucketName: string(streamID.Bucket)}

	exceeded, limit, err := endpoint.projectUsage.ExceedsBandwidthUsage(ctx, keyInfo.ProjectID)
	if err != nil {
		endpoint.log.Error("Retrieving project bandwidth total failed.", zap.Error(err))
	}
	if exceeded {
		endpoint.log.Error("Monthly bandwidth limit exceeded.",
			zap.Stringer("Limit", limit),
			zap.Stringer("Project ID", keyInfo.ProjectID),
		)
		return nil, rpcstatus.Error(rpcstatus.ResourceExhausted, "Exceeded Usage Limit")
	}

	id, err := uuid.FromBytes(streamID.StreamId)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	var segment metabase.Segment
	if req.CursorPosition.PartNumber == 0 && req.CursorPosition.Index == -1 {
		if streamID.MultipartObject {
			return nil, rpcstatus.Error(rpcstatus.Unimplemented, "Used uplink version cannot download multipart objects.")
		}

		segment, err = endpoint.metainfo.metabaseDB.GetLatestObjectLastSegment(ctx, metabase.GetLatestObjectLastSegment{
			ObjectLocation: metabase.ObjectLocation{
				ProjectID:  keyInfo.ProjectID,
				BucketName: string(streamID.Bucket),
				ObjectKey:  metabase.ObjectKey(streamID.EncryptedPath),
			},
		})
	} else {
		segment, err = endpoint.metainfo.metabaseDB.GetSegmentByPosition(ctx, metabase.GetSegmentByPosition{
			StreamID: id,
			Position: metabase.SegmentPosition{
				Part:  uint32(req.CursorPosition.PartNumber),
				Index: uint32(req.CursorPosition.Index),
			},
		})
	}
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	// Update the current bandwidth cache value incrementing the SegmentSize.
	err = endpoint.projectUsage.UpdateProjectBandwidthUsage(ctx, keyInfo.ProjectID, int64(segment.EncryptedSize))
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	encryptedKeyNonce, err := storj.NonceFromBytes(segment.EncryptedKeyNonce)
	if err != nil {
		endpoint.log.Error("unable to get encryption key nonce from metadata", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	if segment.Redundancy.IsZero() { // TODO maybe add method Segment.Inline() bool
		// Inline segment
		err := endpoint.orders.UpdateGetInlineOrder(ctx, bucket, int64(len(segment.InlineData)))
		if err != nil {
			return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
		}
		endpoint.log.Info("Inline Segment Download", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "get"), zap.String("type", "inline"))
		mon.Meter("req_get_inline").Mark(1)

		return &pb.SegmentDownloadResponse{
			SegmentSize:         int64(segment.EncryptedSize),
			EncryptedInlineData: segment.InlineData,

			EncryptedKeyNonce: encryptedKeyNonce,
			EncryptedKey:      segment.EncryptedKey,
		}, nil
	}

	// Remote segment
	limits, privateKey, err := endpoint.orders.CreateGetOrderLimits2(ctx, bucket, segment)
	if err != nil {
		if orders.ErrDownloadFailedNotEnoughPieces.Has(err) {
			endpoint.log.Error("Unable to create order limits.",
				zap.Stringer("Project ID", keyInfo.ProjectID),
				zap.Stringer("API Key ID", keyInfo.ID),
				zap.Error(err),
			)
		}
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	limits = sortLimits(limits, segment)

	// workaround to avoid sending nil values on top level
	for i := range limits {
		if limits[i] == nil {
			limits[i] = &pb.AddressedOrderLimit{}
		}
	}

	endpoint.log.Info("Segment Download", zap.Stringer("Project ID", keyInfo.ProjectID), zap.String("operation", "get"), zap.String("type", "remote"))
	mon.Meter("req_get_remote").Mark(1)

	return &pb.SegmentDownloadResponse{
		AddressedLimits: limits,
		PrivateKey:      privateKey,
		SegmentSize:     int64(segment.EncryptedSize),

		EncryptedKeyNonce: encryptedKeyNonce,
		EncryptedKey:      segment.EncryptedKey,
	}, nil
}

// sortLimits sorts order limits and fill missing ones with nil values.
func sortLimits(limits []*pb.AddressedOrderLimit, segment metabase.Segment) []*pb.AddressedOrderLimit {
	sorted := make([]*pb.AddressedOrderLimit, segment.Redundancy.TotalShares)
	for _, piece := range segment.Pieces {
		sorted[piece.Number] = getLimitByStorageNodeID(limits, piece.StorageNode)
	}
	return sorted
}

func getLimitByStorageNodeID(limits []*pb.AddressedOrderLimit, storageNodeID storj.NodeID) *pb.AddressedOrderLimit {
	for _, limit := range limits {
		if limit == nil || limit.GetLimit() == nil {
			continue
		}

		if limit.GetLimit().StorageNodeId == storageNodeID {
			return limit
		}
	}
	return nil
}

func (endpoint *Endpoint) packStreamID(ctx context.Context, satStreamID *internalpb.StreamID) (streamID storj.StreamID, err error) {
	defer mon.Task()(&ctx)(&err)

	signedStreamID, err := SignStreamID(ctx, endpoint.satellite, satStreamID)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	encodedStreamID, err := pb.Marshal(signedStreamID)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}

	streamID, err = storj.StreamIDFromBytes(encodedStreamID)
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.Internal, err.Error())
	}
	return streamID, nil
}

func (endpoint *Endpoint) packSegmentID(ctx context.Context, satSegmentID *internalpb.SegmentID) (segmentID storj.SegmentID, err error) {
	defer mon.Task()(&ctx)(&err)

	signedSegmentID, err := SignSegmentID(ctx, endpoint.satellite, satSegmentID)
	if err != nil {
		return nil, err
	}

	encodedSegmentID, err := pb.Marshal(signedSegmentID)
	if err != nil {
		return nil, err
	}

	segmentID, err = storj.SegmentIDFromBytes(encodedSegmentID)
	if err != nil {
		return nil, err
	}
	return segmentID, nil
}

func (endpoint *Endpoint) unmarshalSatStreamID(ctx context.Context, streamID storj.StreamID) (_ *internalpb.StreamID, err error) {
	defer mon.Task()(&ctx)(&err)

	satStreamID := &internalpb.StreamID{}
	err = pb.Unmarshal(streamID, satStreamID)
	if err != nil {
		return nil, err
	}

	err = VerifyStreamID(ctx, endpoint.satellite, satStreamID)
	if err != nil {
		return nil, err
	}

	if satStreamID.CreationDate.Before(time.Now().Add(-satIDExpiration)) {
		return nil, errs.New("stream ID expired")
	}

	return satStreamID, nil
}

func (endpoint *Endpoint) unmarshalSatSegmentID(ctx context.Context, segmentID storj.SegmentID) (_ *internalpb.SegmentID, err error) {
	defer mon.Task()(&ctx)(&err)

	satSegmentID := &internalpb.SegmentID{}
	err = pb.Unmarshal(segmentID, satSegmentID)
	if err != nil {
		return nil, err
	}
	if satSegmentID.StreamId == nil {
		return nil, errs.New("stream ID missing")
	}

	err = VerifySegmentID(ctx, endpoint.satellite, satSegmentID)
	if err != nil {
		return nil, err
	}

	if satSegmentID.CreationDate.Before(time.Now().Add(-satIDExpiration)) {
		return nil, errs.New("segment ID expired")
	}

	return satSegmentID, nil
}

// DeleteCommittedObject deletes all the pieces of the storage nodes that belongs
// to the specified object.
//
// NOTE: this method is exported for being able to individually test it without
// having import cycles.
func (endpoint *Endpoint) DeleteCommittedObject(
	ctx context.Context, projectID uuid.UUID, bucket string, object metabase.ObjectKey,
) (deletedObjects []*pb.Object, err error) {
	defer mon.Task()(&ctx, projectID.String(), bucket, object)(&err)

	req := metabase.ObjectLocation{
		ProjectID:  projectID,
		BucketName: bucket,
		ObjectKey:  object,
	}

	result, err := endpoint.metainfo.metabaseDB.DeleteObjectsAllVersions(ctx, metabase.DeleteObjectsAllVersions{Locations: []metabase.ObjectLocation{req}})
	if err != nil {
		return nil, err
	}

	deletedObjects, err = endpoint.deleteObjectsPieces(ctx, result)
	if err != nil {
		endpoint.log.Error("failed to delete pointers",
			zap.Stringer("project", projectID),
			zap.String("bucket", bucket),
			zap.Binary("object", []byte(object)),
			zap.Error(err),
		)
		return deletedObjects, err
	}

	return deletedObjects, nil
}

// DeletePendingObject deletes all the pieces of the storage nodes that belongs
// to the specified pending object.
//
// NOTE: this method is exported for being able to individually test it without
// having import cycles.
func (endpoint *Endpoint) DeletePendingObject(ctx context.Context, projectID uuid.UUID, bucket string, objectKey metabase.ObjectKey, version int32, streamID uuid.UUID) (deletedObjects []*pb.Object, err error) {
	req := metabase.DeletePendingObject{
		ObjectLocation: metabase.ObjectLocation{
			ProjectID:  projectID,
			BucketName: bucket,
			ObjectKey:  objectKey,
		},
		Version:  metabase.Version(version),
		StreamID: streamID,
	}
	result, err := endpoint.metainfo.metabaseDB.DeletePendingObject(ctx, req)
	if err != nil {
		return nil, err
	}

	return endpoint.deleteObjectsPieces(ctx, result)
}

func (endpoint *Endpoint) deleteObjectsPieces(ctx context.Context, result metabase.DeleteObjectResult) (deletedObjects []*pb.Object, err error) {
	// We should ignore client cancelling and always try to delete segments.
	ctx = context2.WithoutCancellation(ctx)

	deletedObjects = make([]*pb.Object, len(result.Objects))
	for i, object := range result.Objects {
		deletedObject, err := endpoint.objectToProto(ctx, object)
		if err != nil {
			return nil, err
		}
		deletedObjects[i] = deletedObject
	}

	endpoint.deleteSegmentPieces(ctx, result.Segments)

	return deletedObjects, nil
}

func (endpoint *Endpoint) deleteSegmentPieces(ctx context.Context, segments []metabase.DeletedSegmentInfo) {
	nodesPieces := groupPiecesByNodeID(segments)

	var requests []piecedeletion.Request
	for node, pieces := range nodesPieces {
		requests = append(requests, piecedeletion.Request{
			Node: storj.NodeURL{
				ID: node,
			},
			Pieces: pieces,
		})
	}

	// Only return an error if we failed to delete the objects. If we failed
	// to delete pieces, let garbage collector take care of it.
	if err := endpoint.deletePieces.Delete(ctx, requests, deleteObjectPiecesSuccessThreshold); err != nil {
		endpoint.log.Error("failed to delete pieces", zap.Error(err))
	}
}

func (endpoint *Endpoint) objectToProto(ctx context.Context, object metabase.Object) (*pb.Object, error) {
	expires := time.Time{}
	if object.ExpiresAt != nil {
		expires = *object.ExpiresAt
	}

	streamID, err := endpoint.packStreamID(ctx, &internalpb.StreamID{
		Bucket:          []byte(object.BucketName),
		EncryptedPath:   []byte(object.ObjectKey),
		Version:         int32(object.Version), // TODO incomatible types
		CreationDate:    time.Now(),
		StreamId:        object.StreamID[:],
		MultipartObject: object.FixedSegmentSize <= 0,
		// TODO: defaultRS may change while the upload is pending.
		// Ideally, we should remove redundancy from satStreamID.
		Redundancy: endpoint.defaultRS,
	})
	if err != nil {
		return nil, err
	}

	var nonce storj.Nonce
	if len(object.EncryptedMetadataNonce) > 0 {
		nonce, err = storj.NonceFromBytes(object.EncryptedMetadataNonce)
		if err != nil {
			return nil, err
		}
	}

	streamMeta := &pb.StreamMeta{}
	err = pb.Unmarshal(object.EncryptedMetadata, streamMeta)
	if err != nil {
		return nil, err
	}

	// TODO is this enough to handle old uplinks
	if streamMeta.EncryptionBlockSize == 0 {
		streamMeta.EncryptionBlockSize = object.Encryption.BlockSize
	}
	if streamMeta.EncryptionType == 0 {
		streamMeta.EncryptionType = int32(object.Encryption.CipherSuite)
	}
	if streamMeta.NumberOfSegments == 0 {
		streamMeta.NumberOfSegments = int64(object.SegmentCount)
	}
	if streamMeta.LastSegmentMeta == nil {
		streamMeta.LastSegmentMeta = &pb.SegmentMeta{
			EncryptedKey: object.EncryptedMetadataEncryptedKey,
			KeyNonce:     object.EncryptedMetadataNonce,
		}
	}

	metadataBytes, err := pb.Marshal(streamMeta)
	if err != nil {
		return nil, err
	}

	result := &pb.Object{
		Bucket:        []byte(object.BucketName),
		EncryptedPath: []byte(object.ObjectKey),
		Version:       int32(object.Version), // TODO incomatible types
		StreamId:      streamID,
		ExpiresAt:     expires,
		CreatedAt:     object.CreatedAt,

		TotalSize: object.TotalEncryptedSize,
		PlainSize: object.TotalPlainSize,

		EncryptedMetadata:             metadataBytes,
		EncryptedMetadataNonce:        nonce,
		EncryptedMetadataEncryptedKey: object.EncryptedMetadataEncryptedKey,
		EncryptionParameters: &pb.EncryptionParameters{
			CipherSuite: pb.CipherSuite(object.Encryption.CipherSuite),
			BlockSize:   int64(object.Encryption.BlockSize),
		},

		// TODO extend DownloadSegment response to provide RS values for client
		RedundancyScheme: endpoint.defaultRS,
	}

	return result, nil
}

func (endpoint *Endpoint) objectEntryToProtoListItem(ctx context.Context, bucket []byte, entry metabase.ObjectEntry) (item *pb.ObjectListItem, err error) {
	expires := time.Time{}
	if entry.ExpiresAt != nil {
		expires = *entry.ExpiresAt
	}

	var nonce storj.Nonce
	if len(entry.EncryptedMetadataNonce) > 0 {
		nonce, err = storj.NonceFromBytes(entry.EncryptedMetadataNonce)
		if err != nil {
			return nil, err
		}
	}

	streamMeta := &pb.StreamMeta{}
	err = pb.Unmarshal(entry.EncryptedMetadata, streamMeta)
	if err != nil {
		return nil, err
	}

	// TODO is this enough to handle old uplinks
	if streamMeta.EncryptionBlockSize == 0 {
		streamMeta.EncryptionBlockSize = entry.Encryption.BlockSize
	}
	if streamMeta.EncryptionType == 0 {
		streamMeta.EncryptionType = int32(entry.Encryption.CipherSuite)
	}
	if streamMeta.NumberOfSegments == 0 {
		streamMeta.NumberOfSegments = int64(entry.SegmentCount)
	}
	if streamMeta.LastSegmentMeta == nil {
		streamMeta.LastSegmentMeta = &pb.SegmentMeta{
			EncryptedKey: entry.EncryptedMetadataEncryptedKey,
			KeyNonce:     entry.EncryptedMetadataNonce,
		}
	}

	metadataBytes, err := pb.Marshal(streamMeta)
	if err != nil {
		return nil, err
	}

	item = &pb.ObjectListItem{
		EncryptedPath:          []byte(entry.ObjectKey),
		Version:                int32(entry.Version), // TODO incomatible types
		Status:                 pb.Object_Status(entry.Status),
		ExpiresAt:              expires,
		CreatedAt:              entry.CreatedAt,
		PlainSize:              entry.TotalPlainSize,
		EncryptedMetadata:      metadataBytes,
		EncryptedMetadataNonce: nonce,
	}

	// Add Stream ID to list items if listing is for pending objects.
	// The client requires the Stream ID to use in the MultipartInfo.
	if entry.Status == metabase.Pending {
		satStreamID, err := endpoint.packStreamID(ctx, &internalpb.StreamID{
			Bucket:        bucket,
			EncryptedPath: item.EncryptedPath,
			Version:       item.Version,
			CreationDate:  item.CreatedAt,
			StreamId:      entry.StreamID[:],
			// TODO: defaultRS may change while the upload is pending.
			// Ideally, we should remove redundancy from satStreamID.
			Redundancy: endpoint.defaultRS,
		})
		if err != nil {
			return nil, err
		}
		item.StreamId = &satStreamID
	}

	return item, nil
}

// groupPiecesByNodeID returns a map that contains pieces with node id as the key.
func groupPiecesByNodeID(segments []metabase.DeletedSegmentInfo) map[storj.NodeID][]storj.PieceID {
	piecesToDelete := map[storj.NodeID][]storj.PieceID{}

	for _, segment := range segments {
		for _, piece := range segment.Pieces {
			pieceID := segment.RootPieceID.Derive(piece.StorageNode, int32(piece.Number))
			piecesToDelete[piece.StorageNode] = append(piecesToDelete[piece.StorageNode], pieceID)
		}
	}

	return piecesToDelete
}

// RevokeAPIKey handles requests to revoke an api key.
func (endpoint *Endpoint) RevokeAPIKey(ctx context.Context, req *pb.RevokeAPIKeyRequest) (resp *pb.RevokeAPIKeyResponse, err error) {
	defer mon.Task()(&ctx)(&err)
	macToRevoke, err := macaroon.ParseMacaroon(req.GetApiKey())
	if err != nil {
		return nil, rpcstatus.Error(rpcstatus.InvalidArgument, "API key to revoke is not a macaroon")
	}
	keyInfo, err := endpoint.validateRevoke(ctx, req.Header, macToRevoke)
	if err != nil {
		return nil, err
	}

	err = endpoint.revocations.Revoke(ctx, macToRevoke.Tail(), keyInfo.ID[:])
	if err != nil {
		endpoint.log.Error("Failed to revoke API key", zap.Error(err))
		return nil, rpcstatus.Error(rpcstatus.Internal, "Failed to revoke API key")
	}

	return &pb.RevokeAPIKeyResponse{}, nil
}

// CreatePath creates a segment key.
func CreatePath(ctx context.Context, projectID uuid.UUID, segmentIndex uint32, bucket, path []byte) (_ metabase.SegmentLocation, err error) {
	// TODO rename to CreateLocation
	defer mon.Task()(&ctx)(&err)
	return metabase.SegmentLocation{
		ProjectID:  projectID,
		BucketName: string(bucket),
		Position:   metabase.SegmentPosition{Index: segmentIndex},
		ObjectKey:  metabase.ObjectKey(path),
	}, nil
}
