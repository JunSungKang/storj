// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package metabase

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zeebo/errs"

	"storj.io/common/storj"
	"storj.io/storj/private/tagsql"
)

// DeleteObjectExactVersion contains arguments necessary for deleting an exact version of object.
type DeleteObjectExactVersion struct {
	Version Version
	ObjectLocation
}

// Verify delete object fields.
func (obj *DeleteObjectExactVersion) Verify() error {
	if err := obj.ObjectLocation.Verify(); err != nil {
		return err
	}
	if obj.Version <= 0 {
		return ErrInvalidRequest.New("Version invalid: %v", obj.Version)
	}
	return nil
}

// DeleteObjectResult result of deleting object.
type DeleteObjectResult struct {
	Segments []DeletedSegmentInfo
}

// DeletedSegmentInfo info about deleted segment.
type DeletedSegmentInfo struct {
	// TODO figure out which part of object are needed to delete from SN
	RootPieceID storj.PieceID
	Pieces      Pieces
}

// DeleteObjectAllVersions contains arguments necessary for deleting all object versions.
type DeleteObjectAllVersions struct {
	ObjectLocation
}

// DeleteObjectLatestVersion contains arguments necessary for deleting latest object version.
type DeleteObjectLatestVersion struct {
	ObjectLocation
}

// DeleteObjectExactVersion deletes an exact object version.
func (db *DB) DeleteObjectExactVersion(ctx context.Context, opts DeleteObjectExactVersion) (result DeleteObjectResult, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return DeleteObjectResult{}, err
	}

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteObjectResult{}, Error.New("failed BeginTx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			err = errs.Combine(err, Error.Wrap(tx.Rollback()))
		}
	}()

	rows, err := tx.Query(ctx, `
		DELETE FROM objects
		WHERE
			project_id   = $1 AND
			bucket_name  = $2 AND
			object_key   = $3 AND
			version      = $4 AND
			status       = 1
		RETURNING stream_id;
	`, opts.ProjectID, opts.BucketName, []byte(opts.ObjectKey), opts.Version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeleteObjectResult{}, Error.New("object missing")
		}
		return DeleteObjectResult{}, Error.New("unable to delete object: %w", err)
	}

	ids, err := scanObjectDeletion(rows)
	if err != nil {
		return DeleteObjectResult{}, err
	}

	if len(ids) == 0 {
		return DeleteObjectResult{}, Error.New("object missing")
	}

	segmentInfos, err := deleteSegments(ctx, tx, ids)
	if err != nil {
		return DeleteObjectResult{}, err
	}

	if len(segmentInfos) != 0 {
		result.Segments = segmentInfos
	}

	err, committed = tx.Commit(), true
	if err != nil {
		return DeleteObjectResult{}, Error.New("unable to commit tx: %w", err)
	}

	return result, nil
}

// DeleteObjectLatestVersion deletes latest object version.
func (db *DB) DeleteObjectLatestVersion(ctx context.Context, opts DeleteObjectLatestVersion) (result DeleteObjectResult, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return DeleteObjectResult{}, err
	}

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteObjectResult{}, Error.New("failed BeginTx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			err = errs.Combine(err, Error.Wrap(tx.Rollback()))
		}
	}()

	// TODO different sql for Postgres and CockroachDB
	// version ONLY for cockroachdb
	// Postgres doesn't support ORDER BY and LIMIT in DELETE
	// rows, err = tx.Query(ctx, `
	// DELETE FROM objects
	// WHERE
	// 	project_id   = $1 AND
	// 	bucket_name  = $2 AND
	// 	object_key   = $3 AND
	// 	status       = 1
	// ORDER BY version DESC
	// LIMIT 1
	// RETURNING stream_id;
	// `, opts.ProjectID, opts.BucketName, opts.ObjectKey)

	// version for Postgres and Cockroachdb (but slow for Cockroachdb)
	rows, err := tx.Query(ctx, `
		DELETE FROM objects
		WHERE
			project_id   = $1 AND
			bucket_name  = $2 AND
			object_key   = $3 AND
			version      = (SELECT version FROM objects WHERE
				project_id   = $1 AND
				bucket_name  = $2 AND
				object_key   = $3 AND
				status       = 1
				ORDER BY version DESC LIMIT 1
			) AND
			status       = 1
		RETURNING stream_id;
	`, opts.ProjectID, opts.BucketName, []byte(opts.ObjectKey))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeleteObjectResult{}, Error.New("object missing")
		}
		return DeleteObjectResult{}, Error.New("unable to delete object: %w", err)
	}

	ids, err := scanObjectDeletion(rows)
	if err != nil {
		return DeleteObjectResult{}, err
	}

	if len(ids) == 0 {
		return DeleteObjectResult{}, Error.New("object missing")
	}

	segmentInfos, err := deleteSegments(ctx, tx, ids)
	if err != nil {
		return DeleteObjectResult{}, err
	}

	if len(segmentInfos) != 0 {
		result.Segments = segmentInfos
	}

	err, committed = tx.Commit(), true
	if err != nil {
		return DeleteObjectResult{}, Error.New("unable to commit tx: %w", err)
	}

	return result, nil
}

// DeleteObjectAllVersions deletes all object versions.
func (db *DB) DeleteObjectAllVersions(ctx context.Context, opts DeleteObjectAllVersions) (result DeleteObjectResult, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return DeleteObjectResult{}, err
	}

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteObjectResult{}, Error.New("failed BeginTx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			err = errs.Combine(err, Error.Wrap(tx.Rollback()))
		}
	}()

	rows, err := tx.Query(ctx, `
		DELETE FROM objects
		WHERE
			project_id   = $1 AND
			bucket_name  = $2 AND
			object_key   = $3 AND
			status       = 1
		RETURNING stream_id;
	`, opts.ProjectID, opts.BucketName, []byte(opts.ObjectKey))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeleteObjectResult{}, Error.New("object missing")
		}
		return DeleteObjectResult{}, Error.New("unable to delete object: %w", err)
	}

	ids, err := scanObjectDeletion(rows)
	if err != nil {
		return DeleteObjectResult{}, err
	}

	if len(ids) == 0 {
		return DeleteObjectResult{}, Error.New("object missing")
	}

	segmentInfos, err := deleteSegments(ctx, tx, ids)
	if err != nil {
		return DeleteObjectResult{}, err
	}

	if len(segmentInfos) != 0 {
		result.Segments = segmentInfos
	}

	err, committed = tx.Commit(), true
	if err != nil {
		return DeleteObjectResult{}, Error.New("unable to commit tx: %w", err)
	}

	return result, nil
}

func scanObjectDeletion(rows tagsql.Rows) (segmentIds []interface{}, err error) {
	defer func() { err = errs.Combine(err, rows.Close()) }()

	ids := make([]interface{}, 0, 10)
	for rows.Next() {
		var streamID []byte
		err = rows.Scan(&streamID)
		if err != nil {
			return []interface{}{}, Error.New("unable to delete object: %w", err)
		}

		ids = append(ids, streamID)
	}

	if err := rows.Err(); err != nil {
		return []interface{}{}, Error.New("unable to delete object: %w", err)
	}
	return ids, nil
}

func deleteSegments(ctx context.Context, tx tagsql.Tx, segmentIds []interface{}) (_ []DeletedSegmentInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	// TODO we need to figure out how integrate this with piece deletion code
	// one issue is that with this approach we need to return all pieces SN ids at once

	infos := make([]DeletedSegmentInfo, 0, len(segmentIds))
	for _, id := range segmentIds {
		segmentsRows, err := tx.Query(ctx, `
			DELETE FROM segments
			WHERE stream_id = $1
			RETURNING root_piece_id, remote_pieces;
		`, id)
		if err != nil {
			return []DeletedSegmentInfo{}, Error.New("unable to delete object: %w", err)
		}

		for segmentsRows.Next() {
			var segmentInfo DeletedSegmentInfo
			err = segmentsRows.Scan(&segmentInfo.RootPieceID, &segmentInfo.Pieces)
			if err != nil {
				return []DeletedSegmentInfo{}, errs.Combine(Error.New("unable to delete object: %w", err), segmentsRows.Close())
			}

			if len(segmentInfo.Pieces) != 0 {
				infos = append(infos, segmentInfo)
			}
		}
		if err := segmentsRows.Err(); err != nil {
			return []DeletedSegmentInfo{}, Error.New("unable to delete object: %w", err)
		}

		if err := segmentsRows.Close(); err != nil {
			return []DeletedSegmentInfo{}, Error.New("unable to delete object: %w", err)
		}
	}
	return infos, nil
}