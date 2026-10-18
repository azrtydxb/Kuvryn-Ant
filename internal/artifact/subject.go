package artifact

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
)

// Subject records an OCI-style association, not a mandatory closure dependency.
// Missing subjects are accepted; an existing target must match its descriptor.
type Subject struct {
	Info      blob.Info
	MediaType string
}

func validSubject(r Publication) bool {
	if r.Subject == nil {
		return r.ArtifactType == "" && len(r.Annotations) == 0
	}
	if r.AssetKey != "manifest" || !r.Subject.Info.Digest.Valid() || r.Subject.Info.Size < 0 || !bounded(r.Subject.MediaType, 256) || len(r.ArtifactType) > 256 {
		return false
	}
	encoded, err := json.Marshal(r.Annotations)
	return err == nil && len(encoded) <= 64<<10
}

// commitSubjectTx participates in publication, including immutable retries. The
// association never crosses the owning component/source or instance boundary.
func commitSubjectTx(ctx context.Context, tx pgx.Tx, r Publication, instance, component, asset string) error {
	if r.Subject == nil {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM artifact_subjects WHERE asset_id=$1)", asset).Scan(&exists); err != nil {
			return ErrUnavailable
		}
		if exists {
			return ErrConflict
		}
		return nil
	}
	var digest, media string
	var size int64
	err := tx.QueryRow(ctx, `SELECT b.digest,b.size_bytes,a.media_type FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND v.version=$2 AND a.asset_key='manifest'`, component, string(r.Subject.Info.Digest)).Scan(&digest, &size, &media)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	if err == nil && (digest != string(r.Subject.Info.Digest) || size != r.Subject.Info.Size || media != r.Subject.MediaType) {
		return blob.ErrIntegrity
	}
	annotations := r.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	encoded, err := json.Marshal(annotations)
	if err != nil {
		return ErrInput
	}
	var matches bool
	err = tx.QueryRow(ctx, `INSERT INTO artifact_subjects(instance_id,asset_id,component_id,subject_digest,subject_size,subject_media_type,artifact_type,annotations)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(asset_id) DO UPDATE SET asset_id=excluded.asset_id
 RETURNING instance_id=$1 AND component_id=$3 AND subject_digest=$4 AND subject_size=$5 AND subject_media_type=$6 AND artifact_type=$7 AND annotations=$8::jsonb`, instance, asset, component, string(r.Subject.Info.Digest), r.Subject.Info.Size, r.Subject.MediaType, r.ArtifactType, encoded).Scan(&matches)
	if err != nil {
		return ErrUnavailable
	}
	if !matches {
		return ErrConflict
	}
	return nil
}
