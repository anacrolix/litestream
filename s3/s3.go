package s3

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/internal"
)

const (
	DefaultRetentionInterval = 1 * time.Hour
)

// MaxKeys is the number of keys S3 can operate on per batch.
const MaxKeys = 1000

var _ litestream.Replica = (*Replica)(nil)

// Replica is a replica that replicates a DB to an S3 bucket.
type Replica struct {
	db       *litestream.DB // source database
	name     string         // replica name, optional
	s3       *s3.S3         // s3 service
	uploader *s3manager.Uploader

	mu  sync.RWMutex
	pos litestream.Pos // last position

	wg     sync.WaitGroup
	cancel func()

	// AWS authentication keys.
	AccessKeyID     string
	SecretAccessKey string

	// S3 bucket information
	Region string
	Bucket string
	Path   string

	// Time to keep snapshots and related WAL files.
	// Database is snapshotted after interval and older WAL files are discarded.
	RetentionInterval time.Duration

	// If true, replica monitors database for changes automatically.
	// Set to false if replica is being used synchronously (such as in tests).
	MonitorEnabled bool
}

// NewReplica returns a new instance of Replica.
func NewReplica(db *litestream.DB, name string) *Replica {
	return &Replica{
		db:     db,
		name:   name,
		cancel: func() {},

		RetentionInterval: DefaultRetentionInterval,
		MonitorEnabled:    true,
	}
}

// Name returns the name of the replica. Returns the type if no name set.
func (r *Replica) Name() string {
	if r.name != "" {
		return r.name
	}
	return r.Type()
}

// Type returns the type of replica.
func (r *Replica) Type() string {
	return "s3"
}

// LastPos returns the last successfully replicated position.
func (r *Replica) LastPos() litestream.Pos {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pos
}

// GenerationDir returns the path to a generation's root directory.
func (r *Replica) GenerationDir(generation string) string {
	return path.Join(r.Path, "generations", generation)
}

// SnapshotDir returns the path to a generation's snapshot directory.
func (r *Replica) SnapshotDir(generation string) string {
	return path.Join(r.GenerationDir(generation), "snapshots")
}

// SnapshotPath returns the path to a snapshot file.
func (r *Replica) SnapshotPath(generation string, index int) string {
	return path.Join(r.SnapshotDir(generation), fmt.Sprintf("%016x.snapshot.gz", index))
}

// MaxSnapshotIndex returns the highest index for the snapshots.
func (r *Replica) MaxSnapshotIndex(generation string) (int, error) {
	snapshots, err := r.Snapshots(context.Background())
	if err != nil {
		return 0, err
	}

	index := -1
	for _, snapshot := range snapshots {
		if snapshot.Generation != generation {
			continue
		} else if index == -1 || snapshot.Index > index {
			index = snapshot.Index
		}
	}
	if index == -1 {
		return 0, fmt.Errorf("no snapshots found")
	}
	return index, nil
}

// WALDir returns the path to a generation's WAL directory
func (r *Replica) WALDir(generation string) string {
	return path.Join(r.GenerationDir(generation), "wal")
}

// Generations returns a list of available generation names.
func (r *Replica) Generations(ctx context.Context) ([]string, error) {
	if err := r.Init(ctx); err != nil {
		return nil, err
	}

	var generations []string
	if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket:    aws.String(r.Bucket),
		Prefix:    aws.String(path.Join(r.Path, "generations") + "/"),
		Delimiter: aws.String("/"),
	}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
		for _, prefix := range page.CommonPrefixes {
			name := path.Base(*prefix.Prefix)
			if !litestream.IsGenerationName(name) {
				continue
			}
			generations = append(generations, name)
		}
		return true
	}); err != nil {
		return nil, err
	}

	return generations, nil
}

// GenerationStats returns stats for a generation.
func (r *Replica) GenerationStats(ctx context.Context, generation string) (stats litestream.GenerationStats, err error) {
	if err := r.Init(ctx); err != nil {
		return stats, err
	}

	// Determine stats for all snapshots.
	n, min, max, err := r.snapshotStats(ctx, generation)
	if err != nil {
		return stats, err
	}
	stats.SnapshotN = n
	stats.CreatedAt, stats.UpdatedAt = min, max

	// Update stats if we have WAL files.
	n, min, max, err = r.walStats(ctx, generation)
	if err != nil {
		return stats, err
	} else if n == 0 {
		return stats, nil
	}

	stats.WALN = n
	if stats.CreatedAt.IsZero() || min.Before(stats.CreatedAt) {
		stats.CreatedAt = min
	}
	if stats.UpdatedAt.IsZero() || max.After(stats.UpdatedAt) {
		stats.UpdatedAt = max
	}
	return stats, nil
}

func (r *Replica) snapshotStats(ctx context.Context, generation string) (n int, min, max time.Time, err error) {
	if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket:    aws.String(r.Bucket),
		Prefix:    aws.String(r.SnapshotDir(generation) + "/"),
		Delimiter: aws.String("/"),
	}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
		for _, obj := range page.Contents {
			if !litestream.IsSnapshotPath(*obj.Key) {
				continue
			}
			modTime := obj.LastModified.UTC()

			n++
			if min.IsZero() || modTime.Before(min) {
				min = modTime
			}
			if max.IsZero() || modTime.After(max) {
				max = modTime
			}
		}
		return true
	}); err != nil {
		return n, min, max, err
	}
	return n, min, max, nil
}

func (r *Replica) walStats(ctx context.Context, generation string) (n int, min, max time.Time, err error) {
	if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket:    aws.String(r.Bucket),
		Prefix:    aws.String(r.WALDir(generation) + "/"),
		Delimiter: aws.String("/"),
	}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
		for _, obj := range page.Contents {
			if !litestream.IsWALPath(*obj.Key) {
				continue
			}
			modTime := obj.LastModified.UTC()

			n++
			if min.IsZero() || modTime.Before(min) {
				min = modTime
			}
			if max.IsZero() || modTime.After(max) {
				max = modTime
			}
		}
		return true
	}); err != nil {
		return n, min, max, err
	}
	return n, min, max, nil
}

// Snapshots returns a list of available snapshots in the replica.
func (r *Replica) Snapshots(ctx context.Context) ([]*litestream.SnapshotInfo, error) {
	if err := r.Init(ctx); err != nil {
		return nil, err
	}

	generations, err := r.Generations(ctx)
	if err != nil {
		return nil, err
	}

	var infos []*litestream.SnapshotInfo
	for _, generation := range generations {
		if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
			Bucket:    aws.String(r.Bucket),
			Prefix:    aws.String(r.SnapshotDir(generation) + "/"),
			Delimiter: aws.String("/"),
		}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
			for _, obj := range page.Contents {
				key := path.Base(*obj.Key)
				index, _, err := litestream.ParseSnapshotPath(key)
				if err != nil {
					continue
				}

				infos = append(infos, &litestream.SnapshotInfo{
					Name:       key,
					Replica:    r.Name(),
					Generation: generation,
					Index:      index,
					Size:       *obj.Size,
					CreatedAt:  obj.LastModified.UTC(),
				})
			}
			return true
		}); err != nil {
			return nil, err
		}
	}

	return infos, nil
}

// WALs returns a list of available WAL files in the replica.
func (r *Replica) WALs(ctx context.Context) ([]*litestream.WALInfo, error) {
	if err := r.Init(ctx); err != nil {
		return nil, err
	}

	generations, err := r.Generations(ctx)
	if err != nil {
		return nil, err
	}

	var infos []*litestream.WALInfo
	for _, generation := range generations {
		var prev *litestream.WALInfo
		if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
			Bucket:    aws.String(r.Bucket),
			Prefix:    aws.String(r.WALDir(generation) + "/"),
			Delimiter: aws.String("/"),
		}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
			for _, obj := range page.Contents {
				key := path.Base(*obj.Key)

				index, offset, _, _, err := litestream.ParseWALPath(key)
				if err != nil {
					continue
				}

				// Update previous record if generation & index match.
				if prev != nil && prev.Index == index {
					prev.Size += *obj.Size
					prev.CreatedAt = obj.LastModified.UTC()
					continue
				}

				// Append new WAL record and keep reference to append additional
				// size for segmented WAL files.
				prev = &litestream.WALInfo{
					Name:       key,
					Replica:    r.Name(),
					Generation: generation,
					Index:      index,
					Offset:     offset,
					Size:       *obj.Size,
					CreatedAt:  obj.LastModified.UTC(),
				}
				infos = append(infos, prev)
			}
			return true
		}); err != nil {
			return nil, err
		}
	}

	return infos, nil
}

// Start starts replication for a given generation.
func (r *Replica) Start(ctx context.Context) {
	// Ignore if replica is being used sychronously.
	if !r.MonitorEnabled {
		return
	}

	// Stop previous replication.
	r.Stop()

	// Wrap context with cancelation.
	ctx, r.cancel = context.WithCancel(ctx)

	// Start goroutines to manage replica data.
	r.wg.Add(2)
	go func() { defer r.wg.Done(); r.monitor(ctx) }()
	go func() { defer r.wg.Done(); r.retainer(ctx) }()
}

// Stop cancels any outstanding replication and blocks until finished.
func (r *Replica) Stop() {
	r.cancel()
	r.wg.Wait()
}

// monitor runs in a separate goroutine and continuously replicates the DB.
func (r *Replica) monitor(ctx context.Context) {
	// Continuously check for new data to replicate.
	ch := make(chan struct{})
	close(ch)
	var notify <-chan struct{} = ch

	for {
		select {
		case <-ctx.Done():
			return
		case <-notify:
		}

		// Fetch new notify channel before replicating data.
		notify = r.db.Notify()

		// Synchronize the shadow wal into the replication directory.
		if err := r.Sync(ctx); err != nil {
			log.Printf("%s(%s): sync error: %s", r.db.Path(), r.Name(), err)
			continue
		}
	}
}

// retainer runs in a separate goroutine and handles retention.
func (r *Replica) retainer(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.EnforceRetention(ctx); err != nil {
				log.Printf("%s(%s): retain error: %s", r.db.Path(), r.Name(), err)
				continue
			}
		}
	}
}

// CalcPos returns the position for the replica for the current generation.
// Returns a zero value if there is no active generation.
func (r *Replica) CalcPos(generation string) (pos litestream.Pos, err error) {
	if err := r.Init(context.Background()); err != nil {
		return pos, err
	}

	pos.Generation = generation

	// Find maximum snapshot index.
	if pos.Index, err = r.MaxSnapshotIndex(generation); err != nil {
		return litestream.Pos{}, err
	}

	index := -1
	var offset int64
	if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket:    aws.String(r.Bucket),
		Prefix:    aws.String(r.WALDir(generation) + "/"),
		Delimiter: aws.String("/"),
	}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
		for _, obj := range page.Contents {
			key := path.Base(*obj.Key)

			idx, offset, sz, _, err := litestream.ParseWALPath(key)
			if err != nil {
				continue // invalid wal filename
			}

			if index == -1 || idx > index {
				index, offset = idx, 0 // start tracking new wal
			} else if idx == index {
				offset += sz // append additional size to wal
			}
		}
		return true
	}); err != nil {
		return litestream.Pos{}, err
	}
	if index == -1 {
		return pos, nil // no wal files
	}
	pos.Index = index
	pos.Offset = offset

	return pos, nil
}

// snapshot copies the entire database to the replica path.
func (r *Replica) snapshot(ctx context.Context, generation string, index int) error {
	// Acquire a read lock on the database during snapshot to prevent checkpoints.
	tx, err := r.db.SQLDB().Begin()
	if err != nil {
		return err
	} else if _, err := tx.ExecContext(ctx, `SELECT COUNT(1) FROM _litestream_seq;`); err != nil {
		tx.Rollback()
		return err
	}
	defer tx.Rollback()

	// Open database file handle.
	f, err := os.Open(r.db.Path())
	if err != nil {
		return err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	gw := gzip.NewWriter(pw)
	go func() {
		if _, err := io.Copy(gw, f); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.CloseWithError(gw.Close())
	}()

	snapshotPath := r.SnapshotPath(generation, index)

	if _, err := r.uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(r.Bucket),
		Key:    aws.String(snapshotPath),
		Body:   pr,
	}); err != nil {
		return err
	}

	return nil
}

// snapshotN returns the number of snapshots for a generation.
func (r *Replica) snapshotN(generation string) (int, error) {
	snapshots, err := r.Snapshots(context.Background())
	if err != nil {
		return 0, err
	}

	var n int
	for _, snapshot := range snapshots {
		if snapshot.Generation == generation {
			n++
		}
	}
	return n, nil
}

// Init initializes the connection to S3. No-op if already initialized.
func (r *Replica) Init(ctx context.Context) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.s3 != nil {
		return nil
	}

	sess, err := session.NewSession(&aws.Config{
		Credentials: credentials.NewStaticCredentials(r.AccessKeyID, r.SecretAccessKey, ""),
		Region:      aws.String(r.Region),
	})
	if err != nil {
		return fmt.Errorf("cannot create aws session: %w", err)
	}
	r.s3 = s3.New(sess)
	r.uploader = s3manager.NewUploader(sess)
	return nil
}

func (r *Replica) Sync(ctx context.Context) (err error) {
	if err := r.Init(ctx); err != nil {
		return err
	}

	// Find current position of database.
	dpos, err := r.db.Pos()
	if err != nil {
		return fmt.Errorf("cannot determine current generation: %w", err)
	} else if dpos.IsZero() {
		return fmt.Errorf("no generation, waiting for data")
	}
	generation := dpos.Generation

	// Create snapshot if no snapshots exist for generation.
	if n, err := r.snapshotN(generation); err != nil {
		return err
	} else if n == 0 {
		if err := r.snapshot(ctx, generation, dpos.Index); err != nil {
			return err
		}
	}

	// Determine position, if necessary.
	if r.LastPos().IsZero() {
		pos, err := r.CalcPos(generation)
		if err != nil {
			return fmt.Errorf("cannot determine replica position: %s", err)
		}

		r.mu.Lock()
		r.pos = pos
		r.mu.Unlock()
	}

	// Read all WAL files since the last position.
	for {
		if err = r.syncWAL(ctx); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
	}

	return nil
}

func (r *Replica) syncWAL(ctx context.Context) (err error) {
	rd, err := r.db.ShadowWALReader(r.LastPos())
	if err == io.EOF {
		return err
	} else if err != nil {
		return fmt.Errorf("wal reader: %w", err)
	}
	defer rd.Close()

	// Read to intermediate buffer to determine size.
	b, err := ioutil.ReadAll(rd)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		return err
	}

	// Build a WAL path with the index/offset as well as size so we can ensure
	// that files are contiguous without having to decompress.
	walPath := path.Join(
		r.WALDir(rd.Pos().Generation),
		litestream.FormatWALPathWithOffsetSize(rd.Pos().Index, rd.Pos().Offset, int64(len(b)))+".gz",
	)

	if _, err := r.uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(r.Bucket),
		Key:    aws.String(walPath),
		Body:   &buf,
	}); err != nil {
		return err
	}

	// Save last replicated position.
	r.mu.Lock()
	r.pos = rd.Pos()
	r.mu.Unlock()

	return nil
}

// SnapshotReader returns a reader for snapshot data at the given generation/index.
func (r *Replica) SnapshotReader(ctx context.Context, generation string, index int) (io.ReadCloser, error) {
	if err := r.Init(ctx); err != nil {
		return nil, err
	}

	// Pipe download to return an io.Reader.
	out, err := r.s3.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.Bucket),
		Key:    aws.String(r.SnapshotPath(generation, index)),
	})
	if err != nil {
		return nil, err
	}

	// Decompress the snapshot file.
	gr, err := gzip.NewReader(out.Body)
	if err != nil {
		out.Body.Close()
		return nil, err
	}
	return internal.NewReadCloser(gr, out.Body), nil
}

// WALReader returns a reader for WAL data at the given index.
// Returns os.ErrNotExist if no matching index is found.
func (r *Replica) WALReader(ctx context.Context, generation string, index int) (io.ReadCloser, error) {
	if err := r.Init(ctx); err != nil {
		return nil, err
	}

	// Collect all files for the index.
	var keys []string
	var offset int64
	var innerErr error
	if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket: aws.String(r.Bucket),
		Prefix: aws.String(path.Join(r.WALDir(generation), fmt.Sprintf("%016x_", index))),
	}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
		for _, obj := range page.Contents {
			// Read the offset & size from the filename. We need to check this
			// against a running offset to ensure there are no gaps.
			_, off, sz, _, err := litestream.ParseWALPath(path.Base(*obj.Key))
			if err != nil {
				continue
			} else if off != offset {
				innerErr = fmt.Errorf("out of sequence wal segments: %s/%016x", generation, index)
				return false
			}

			keys = append(keys, *obj.Key)
			offset += sz
		}
		return true
	}); err != nil {
		return nil, err
	} else if innerErr != nil {
		return nil, innerErr
	}

	// Open each file and concatenate into a multi-reader.
	var mrc multiReadCloser
	for _, key := range keys {
		// Pipe download to return an io.Reader.
		out, err := r.s3.GetObjectWithContext(ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			mrc.Close()
			return nil, err
		}

		// Decompress the snapshot file.
		gr, err := gzip.NewReader(out.Body)
		if err != nil {
			out.Body.Close()
			mrc.Close()
			return nil, err
		}

		mrc.readers = append(mrc.readers, internal.NewReadCloser(gr, out.Body))
	}

	return &mrc, nil
}

// EnforceRetention forces a new snapshot once the retention interval has passed.
// Older snapshots and WAL files are then removed.
func (r *Replica) EnforceRetention(ctx context.Context) (err error) {
	if err := r.Init(ctx); err != nil {
		return err
	}

	// Find current position of database.
	pos, err := r.db.Pos()
	if err != nil {
		return fmt.Errorf("cannot determine current generation: %w", err)
	} else if pos.IsZero() {
		return fmt.Errorf("no generation, waiting for data")
	}

	// Obtain list of snapshots that are within the retention period.
	snapshots, err := r.Snapshots(ctx)
	if err != nil {
		return fmt.Errorf("cannot obtain snapshot list: %w", err)
	}
	snapshots = litestream.FilterSnapshotsAfter(snapshots, time.Now().Add(-r.RetentionInterval))

	// If no retained snapshots exist, create a new snapshot.
	if len(snapshots) == 0 {
		log.Printf("%s(%s): snapshots exceeds retention, creating new snapshot", r.db.Path(), r.Name())
		if err := r.snapshot(ctx, pos.Generation, pos.Index); err != nil {
			return fmt.Errorf("cannot snapshot: %w", err)
		}
		snapshots = append(snapshots, &litestream.SnapshotInfo{Generation: pos.Generation, Index: pos.Index})
	}

	// Loop over generations and delete unretained snapshots & WAL files.
	generations, err := r.Generations(ctx)
	if err != nil {
		return fmt.Errorf("cannot obtain generations: %w", err)
	}
	for _, generation := range generations {
		// Find earliest retained snapshot for this generation.
		snapshot := litestream.FindMinSnapshotByGeneration(snapshots, generation)

		// Delete generations if it has no snapshots being retained.
		if snapshot == nil {
			log.Printf("%s(%s): generation %q has no retained snapshots, deleting", r.db.Path(), r.Name(), generation)
			if err := r.deleteGenerationBefore(ctx, generation, -1); err != nil {
				return fmt.Errorf("cannot delete generation %q dir: %w", generation, err)
			}
			continue
		}

		// Otherwise delete all snapshots & WAL files before a lowest retained index.
		if err := r.deleteGenerationBefore(ctx, generation, snapshot.Index); err != nil {
			return fmt.Errorf("cannot delete generation %q files before index %d: %w", generation, snapshot.Index, err)
		}
	}

	return nil
}

func (r *Replica) deleteGenerationBefore(ctx context.Context, generation string, index int) (err error) {
	// Collect all files for the generation.
	var objIDs []*s3.ObjectIdentifier
	if err := r.s3.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket: aws.String(r.Bucket),
		Prefix: aws.String(r.GenerationDir(generation)),
	}, func(page *s3.ListObjectsOutput, lastPage bool) bool {
		for _, obj := range page.Contents {
			// Skip snapshots or WALs that are after the search index unless -1.
			if index != -1 {
				if idx, _, err := litestream.ParseSnapshotPath(path.Base(*obj.Key)); err == nil && idx >= index {
					continue
				} else if idx, _, _, _, err := litestream.ParseWALPath(path.Base(*obj.Key)); err == nil && idx >= index {
					continue
				}
			}

			objIDs = append(objIDs, &s3.ObjectIdentifier{Key: obj.Key})
		}
		return true
	}); err != nil {
		return err
	}

	// Delete all files in batches.
	for i := 0; i < len(objIDs); i += MaxKeys {
		j := i + MaxKeys
		if j > len(objIDs) {
			j = len(objIDs)
		}

		for _, objID := range objIDs[i:j] {
			log.Printf("%s(%s): generation %q file no longer retained, deleting %s", r.db.Path(), r.Name(), generation, path.Base(*objID.Key))
		}

		if _, err := r.s3.DeleteObjectsWithContext(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(r.Bucket),
			Delete: &s3.Delete{
				Objects: objIDs[i:j],
				Quiet:   aws.Bool(true),
			},
		}); err != nil {
			return err
		}
	}

	return nil
}

type multiReadCloser struct {
	readers []io.ReadCloser
}

func (mr *multiReadCloser) Read(p []byte) (n int, err error) {
	for len(mr.readers) > 0 {
		n, err = mr.readers[0].Read(p)
		if err == io.EOF {
			if e := mr.readers[0].Close(); e != nil {
				return n, e
			}
			mr.readers[0] = nil
			mr.readers = mr.readers[1:]
		}

		if n > 0 || err != io.EOF {
			if err == io.EOF && len(mr.readers) > 0 {
				err = nil
			}
			return
		}
	}
	return 0, io.EOF
}

func (mr *multiReadCloser) Close() (err error) {
	for _, r := range mr.readers {
		if e := r.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}