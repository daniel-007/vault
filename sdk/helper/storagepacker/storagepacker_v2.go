package storagepacker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	radix "github.com/armon/go-radix"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/errwrap"
	log "github.com/hashicorp/go-hclog"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault/sdk/helper/compressutil"
	"github.com/hashicorp/vault/sdk/helper/cryptoutil"
	"github.com/hashicorp/vault/sdk/helper/locksutil"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/hashicorp/vault/sdk/physical"
	"sort"
)

const (
	defaultBaseBucketBits  = 8
	defaultBucketShardBits = 4
)

type Config struct {
	// BucketStorageView is the storage to be used by all the buckets
	BucketStorageView *logical.StorageView `json:"-"`

	// ConfigStorageView is the storage to store config info
	ConfigStorageView *logical.StorageView `json:"-"`

	// Logger for output
	Logger log.Logger `json:"-"`

	// BaseBucketBits is the number of bits to use for buckets at the base level
	BaseBucketBits int `json:"base_bucket_bits"`

	// BucketShardBits is the number of bits to use for sub-buckets a bucket
	// gets sharded into when it reaches the maximum threshold.
	BucketShardBits int `json:"-"`
}

// StoragePacker packs many items into abstractions called buckets. The goal
// is to employ a reduced number of storage entries for a relatively huge
// number of items. This is the second version of the utility which supports
// indefinitely expanding the capacity of the storage by sharding the buckets
// when they exceed the imposed limit.
//
// The locking discipline for StoragePackerV2:
// * Acquire locks in the order storageLocks < shardLock < bucketsCacheLock to avoid
//   deadlock.
// * Hold the storageLock for the duration of any read or write operation on a given
//   bucket. The storageLock is idetermined by the "cache key" of the bucket, which
//   omits all of the '/'s.
//
// Invariant: a Bucket has a nil ItemMap if it has child shards.
// Its ItemMap is non-nil if it is merely empty. This lets us
// tell whether a bucket has been split since we first retrieved it.
type StoragePackerV2 struct {
	*Config
	storageLocks []*locksutil.LockEntry

	bucketsCache     *radix.Tree
	bucketsCacheLock sync.RWMutex

	queueMode     uint32
	queuedBuckets sync.Map

	// disableSharding is used for tests
	disableSharding bool

	// Ensures we only process one sharding at a time, so that we can't grab
	// the same locks for the child buckets
	shardLock sync.RWMutex

	prewarmCache sync.Once
}

// LockedBucket embeds a bucket and its corresponding lock to ensure thread
// safety
type LockedBucket struct {
	*locksutil.LockEntry
	*Bucket
}

type lockOperation func(*LockedBucket)

// Safely identify the bucket for a given key, and aquire its storage lock
// The key may be the full hash of the item ID, or a prefix, with a bucket Key with embedded /'s
// The cache may initially be empty, so "not found" is acceptable, and needs to be handled
// by deciding whether to create the bucket.
//
// This function uses the same logic whether we want a read or a write operation.
// Use the helpers below that set everything up nicely for read locks or write locks.
func (s *StoragePackerV2) lockBucket(key string, acquire lockOperation, release lockOperation) (bucket *LockedBucket, found bool, err error) {
	cacheKey := s.GetCacheKey(key)
	lastCacheKey := ""

	for true {
		s.bucketsCacheLock.RLock()
		keyPrefix, bucketRaw, found := s.bucketsCache.LongestPrefix(cacheKey)
		s.bucketsCacheLock.RUnlock()

		if !found {
			return nil, false, nil
		}

		bucket := bucketRaw.(*LockedBucket)
		acquire(bucket)
		if bucket.ItemMap != nil {
			// Lock still held on return
			return bucket, true, nil
		} else if keyPrefix != lastCacheKey {
			// Try again, we moved down the tree
			lastCacheKey = keyPrefix
			release(bucket)
		} else {
			release(bucket)
			return nil, true, errors.New("Bucket " + bucket.Key + " has nil ItemMap")
		}
	}
	// Not possible
	return nil, false, nil
}

func readAcquire(b *LockedBucket) {
	b.RLock()
}
func readRelease(b *LockedBucket) {
	b.RUnlock()
}
func writeAcquire(b *LockedBucket) {
	b.Lock()
}
func writeRelease(b *LockedBucket) {
	b.Unlock()
}

func (s *StoragePackerV2) lockBucketForRead(key string) (bucket *LockedBucket, found bool, err error) {
	return s.lockBucket(key, readAcquire, readRelease)
}

func (s *StoragePackerV2) lockBucketForWrite(key string) (bucket *LockedBucket, found bool, err error) {
	return s.lockBucket(key, writeAcquire, writeRelease)
}

// If an entry is not found in bucketsCache, it might be because it's never
// been created. Try to load it from storage first, then create it if desired.
// This only works for the first tier of buckets, without the cache we cant'
// tell how long the prefix should be.
// FIXME: what cases does loading from storage cover?
// Will return with a write lock held, or nil if no storage exists.
func (s *StoragePackerV2) handleCacheMiss(ctx context.Context, key string, create bool, skipCache bool) (bucket *LockedBucket, err error) {
	cacheKey := s.GetCacheKey(key)
	rootShardLength := s.BaseBucketBits / 4
	if len(cacheKey) < rootShardLength {
		return nil, errors.New("Key too short.")
	}
	firstBucketKey := cacheKey[0 : s.BaseBucketBits/4]

	// Grab the lock for this not-yet-existent bucket
	lock := locksutil.LockForKey(s.storageLocks, firstBucketKey)
	lock.Lock()

	// Re-check radix tree (since the bucket could be there now.)
	s.bucketsCacheLock.RLock()
	_, _, found := s.bucketsCache.LongestPrefix(firstBucketKey)
	s.bucketsCacheLock.RUnlock()

	if found && !skipCache {
		// OK, we found it but it is the right bucket after all?
		// For example, the bucket could have been created *and* split since
		// we got the initial cache miss.
		// Time to start over and do the lookup again.
		// (This will be safe if the cache only grows, never shrinks.)
		lock.Unlock()
		bucket, found, err := s.lockBucketForWrite(key) // original key!
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("Bucket found in handleCacheMiss disappeared.")
		}
		return bucket, nil
	}

	// Read from the underlying view
	storageEntry, err := s.BucketStorageView.Get(ctx, firstBucketKey)
	if err != nil {
		return nil, errwrap.Wrapf("failed to read packed storage entry: {{err}}", err)
	}
	if storageEntry != nil {
		bucket, err := s.DecodeBucket(storageEntry)
		if err != nil {
			return nil, err
		}
		// TODO: check that the lock created is the same?

		s.bucketsCacheLock.Lock()
		s.bucketsCache.Insert(firstBucketKey, bucket)
		s.bucketsCacheLock.Unlock()
		return bucket, nil
	}
	if !create {
		lock.Unlock()
		return nil, nil
	}
	bucket = &LockedBucket{
		LockEntry: lock,
		Bucket: &Bucket{
			Key:     firstBucketKey,
			ItemMap: make(map[string][]byte),
		},
	}
	s.bucketsCacheLock.Lock()
	s.bucketsCache.Insert(firstBucketKey, bucket)
	s.bucketsCacheLock.Unlock()

	// Not yet persisted, we will do that after the first upsert
	return bucket, nil
}

func (s *StoragePackerV2) BucketsView() *logical.StorageView {
	return s.BucketStorageView
}

func (s *StoragePackerV2) BucketStorageKeyForItemID(itemID string) string {
	hexVal := GetItemIDHash(itemID)

	s.bucketsCacheLock.RLock()
	_, bucketRaw, found := s.bucketsCache.LongestPrefix(hexVal)
	s.bucketsCacheLock.RUnlock()

	if found {
		return bucketRaw.(*LockedBucket).Key
	}

	// If we have existing buckets we'd have parsed them in on startup so this
	// is a fresh storagepacker, so we use the root bits to return a proper
	// number of chars.
	return hexVal[0 : s.BaseBucketBits/4]
}

func (s *StoragePackerV2) BucketKey(itemID string) string {
	return s.GetCacheKey(s.BucketStorageKeyForItemID(itemID))
}

func (s *StoragePackerV2) GetCacheKey(key string) string {
	return strings.Replace(key, "/", "", -1)
}

func GetItemIDHash(itemID string) string {
	return hex.EncodeToString(cryptoutil.Blake2b256Hash(itemID))
}

func (s *StoragePackerV2) visitDiskBucketsInOrder(ctx context.Context, keys []string) error {
	// If we crash while writing out new buckets created by sharding,
	// we will have version N of the bucket in storage, and version N+1
	// represented in the sub-buckets.
	//
	// To bring these two into alignment, we can roll back to version N.
	// It might be possible to foll forward in some cases (particularly if
	// only one item can be added at a time?) but the crash must have
	// occurred before PutBucket() returned, so the new data need not be saved.
	//
	// If we load a bucket that has sub-buckets but is non-empty,
	// delete all of the sub-buckets.  (The current implementation recurses
	// so there may be more than one level of sub-buckets created by shardBucket.)

	// Visit the buckets using inorder traversal, parents before children.
	sort.Strings(keys)

	var nonemptyParent string
	nonemptyParent = "NOT_A_PREFIX"

	for _, key := range keys {
		if strings.HasPrefix(key, nonemptyParent) {
			// FIXME: can I give more context about the storage path?
			s.Logger.Warn("Detected shadowed bucket, removing", "key", key, "parent", nonemptyParent)
			s.BucketStorageView.Delete(ctx, key)
			continue
		}

		// Read from the underlying view
		storageEntry, err := s.BucketStorageView.Get(ctx, key)
		if err != nil {
			return errwrap.Wrapf("failed to read packed storage entry: {{err}}", err)
		}
		if storageEntry == nil {
			return fmt.Errorf("no data found at bucket %s", key)
		}
		bucket, err := s.DecodeBucket(storageEntry)
		if err != nil {
			return err
		}
		if bucket.ItemMap != nil {
			nonemptyParent = key
		}

		s.bucketsCacheLock.Lock()
		s.bucketsCache.Insert(s.GetCacheKey(bucket.Key), bucket)
		s.bucketsCacheLock.Unlock()
	}
	return nil
}

func (s *StoragePackerV2) BucketKeys(ctx context.Context) ([]string, error) {
	var retErr error
	s.prewarmCache.Do(func() {
		diskBuckets, err := logical.CollectKeys(ctx, s.BucketStorageView)
		if err != nil {
			retErr = err
			return
		}
		retErr = s.visitDiskBucketsInOrder(ctx, diskBuckets)
	})
	if retErr != nil {
		return nil, retErr
	}

	ret := make([]string, 0, 256)
	s.bucketsCacheLock.RLock()
	s.bucketsCache.Walk(func(s string, _ interface{}) bool {
		ret = append(ret, s)
		return false
	})
	s.bucketsCacheLock.RUnlock()

	return ret, nil
}

// Get returns a bucket for a given key
func (s *StoragePackerV2) GetBucket(ctx context.Context, key string, skipCache bool) (*LockedBucket, error) {
	bucket, found, err := s.lockBucketForRead(key)
	if err != nil {
		return nil, err
	}
	if found {
		bucket.RUnlock()
		if !skipCache {
			return bucket, nil
		}
	}
	// Don't create if missing
	bucket, err = s.handleCacheMiss(ctx, key, false, skipCache)
	if bucket != nil {
		bucket.Unlock()
	}
	return bucket, err
}

// NOTE: Don't put inserting into the cache here, as that will mess with
// upgrade cases for the identity store as we want to keep the bucket out of
// the cache until we actually re-store it.
func (s *StoragePackerV2) DecodeBucket(storageEntry *logical.StorageEntry) (*LockedBucket, error) {
	uncompressedData, notCompressed, err := compressutil.Decompress(storageEntry.Value)
	if err != nil {
		return nil, errwrap.Wrapf("failed to decompress packed storage entry: {{err}}", err)
	}
	if notCompressed {
		uncompressedData = storageEntry.Value
	}

	var bucket Bucket
	err = proto.Unmarshal(uncompressedData, &bucket)
	if err != nil {
		return nil, errwrap.Wrapf("failed to decode packed storage entry: {{err}}", err)
	}

	cacheKey := s.GetCacheKey(storageEntry.Key)
	lock := locksutil.LockForKey(s.storageLocks, cacheKey)
	lb := &LockedBucket{
		LockEntry: lock,
		Bucket:    &bucket,
	}
	lb.Key = storageEntry.Key

	return lb, nil
}

// Put stores a packed bucket entry
func (s *StoragePackerV2) PutBucket(ctx context.Context, bucket *LockedBucket) error {
	if bucket == nil {
		return fmt.Errorf("nil bucket entry")
	}

	if bucket.Key == "" {
		return fmt.Errorf("missing key")
	}

	// FIXME: don't trust the passed-in object?
	bucket.Lock()
	defer bucket.Unlock()

	if err := s.storeBucket(ctx, bucket, true); err != nil {
		return errwrap.Wrapf("failed at high level bucket put: {{err}}", err)
	}

	s.bucketsCacheLock.Lock()
	s.bucketsCache.Insert(s.GetCacheKey(bucket.Key), bucket)
	s.bucketsCacheLock.Unlock()

	return nil
}

func (s *StoragePackerV2) shardBucket(ctx context.Context, bucket *LockedBucket, cacheKey string, allowLocking bool) error {
	if allowLocking {
		s.shardLock.Lock()
		defer s.shardLock.Unlock()
	}

	numShards := int(math.Pow(2.0, float64(s.BucketShardBits)))

	// Create the shards
	s.Logger.Info("sharding bucket", "bucket_key", bucket.Key, "num_shards", numShards)
	defer s.Logger.Info("sharding bucket process exited", "bucket_key", bucket.Key)

	shards := make(map[string]*LockedBucket, numShards)
	for i := 0; i < numShards; i++ {
		shardKey := fmt.Sprintf("%x", i)
		lock := locksutil.LockForKey(s.storageLocks, cacheKey+shardKey)
		shardedBucket := &LockedBucket{
			LockEntry: lock,
			Bucket: &Bucket{
				Key:     fmt.Sprintf("%s/%s", bucket.Key, shardKey),
				ItemMap: make(map[string][]byte),
			},
		}
		shards[shardKey] = shardedBucket
		s.Logger.Trace("created shard", "shard_key", shardKey)
	}

	s.Logger.Debug("resilvering items")

	parentPrefix := s.GetCacheKey(bucket.Key)
	// Resilver the items
	for k, v := range bucket.ItemMap {
		itemKey := strings.TrimPrefix(k, parentPrefix)[0 : s.BucketShardBits/4]
		s.Logger.Trace("resilvering item", "parent_prefix", parentPrefix, "item_id", k, "item_key", itemKey)
		// Sanity check
		childBucket, ok := shards[itemKey]
		if !ok {
			// We didn't complete sharding so don't make other parts of the
			// code think that it completed
			s.Logger.Error("failed to find sharded storagepacker bucket", "bucket_key", bucket.Key, "item_key", itemKey)
			return errors.New("failed to shard storagepacker bucket")
		}
		childBucket.ItemMap[k] = v
	}

	s.Logger.Debug("storing sharded buckets")

	// Ensure we can write all of these buckets. Create a cleanup function if not.
	retErr := new(multierror.Error)
	cleanupStorage := func() {
		for _, v := range shards {
			if err := s.BucketStorageView.Delete(ctx, v.Key); err != nil {
				retErr = multierror.Append(retErr, err)
				// Don't exit out, clean up as much as possible
			}
		}
	}
	for k, v := range shards {
		s.Logger.Trace("storing bucket", "shard", k)
		// allowLocking = false because we might recursively break
		// up one of the shards
		if err := s.storeBucket(ctx, v, false); err != nil {
			s.Logger.Debug("encountered error", "shard", k)
			retErr = multierror.Append(retErr, err)
			cleanupStorage()
			return retErr
		}
	}

	cleanupCache := func() {
		for _, v := range shards {
			s.bucketsCache.Delete(s.GetCacheKey(v.Key))
		}
	}

	// Update the original and persist it.
	// Prior to this point, version N is in storage and we revert back to it.
	// After this point, storage is in version N+1 but the cache has not yet
	// been updated; anybody trying to access one of the keys will hit
	// this bucket and block on the lock.
	origBucketItemMap := bucket.ItemMap
	bucket.ItemMap = nil
	if err := s.storeBucket(ctx, bucket, false); err != nil {
		retErr = multierror.Append(retErr, err)
		bucket.ItemMap = origBucketItemMap
		cleanupStorage()
		cleanupCache()
		return retErr
	}

	// Add to the cache.
	s.Logger.Debug("updating cache")
	s.bucketsCacheLock.Lock()
	for _, v := range shards {
		s.bucketsCache.Insert(s.GetCacheKey(v.Key), v)
	}
	s.bucketsCacheLock.Unlock()

	return retErr.ErrorOrNil()
}

// storeBucket actually stores the bucket. It expects that it's already locked.
func (s *StoragePackerV2) storeBucket(ctx context.Context, bucket *LockedBucket, allowLocking bool) error {
	if atomic.LoadUint32(&s.queueMode) == 1 {
		s.queuedBuckets.Store(bucket.Key, bucket)
		return nil
	}

	marshaledBucket, err := proto.Marshal(bucket.Bucket)
	if err != nil {
		return errwrap.Wrapf("failed to marshal bucket: {{err}}", err)
	}

	compressedBucket, err := compressutil.Compress(marshaledBucket, &compressutil.CompressionConfig{
		Type: compressutil.CompressionTypeSnappy,
	})
	if err != nil {
		return errwrap.Wrapf("failed to compress packed bucket: {{err}}", err)
	}

	// Store the compressed value
	err = s.BucketStorageView.Put(ctx, &logical.StorageEntry{
		Key:   bucket.Key,
		Value: compressedBucket,
	})
	if err != nil {
		if strings.Contains(err.Error(), physical.ErrValueTooLarge) && !s.disableSharding {
			err = s.shardBucket(ctx, bucket, s.GetCacheKey(bucket.Key), allowLocking)
		}
		if err != nil {
			return errwrap.Wrapf("failed to persist packed storage entry: {{err}}", err)
		}
	}

	return nil
}

// DeleteBucket deletes an entire bucket entry
// To maintain the tree of shards' invariants, it seems better to maintain the bucket
// in storage but empty it. FIXME: does this violate the intended use cases?
func (s *StoragePackerV2) DeleteBucket(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("missing key")
	}

	bucket, found, err := s.lockBucketForWrite(key)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	defer bucket.Unlock()

	if bucket.ItemMap == nil {
		// Could do a recursive delete instead.
		return fmt.Errorf("Bucket %s has shards, no items deleted.", bucket.Key)
	}

	bucket.ItemMap = make(map[string][]byte)
	if err := s.storeBucket(ctx, bucket, true); err != nil {
		return errwrap.Wrapf("failed to write deleted bucket: {{err}}", err)
	}
	return nil
}

// upsert either inserts a new item into the bucket or updates an existing one
// if an item with a matching key is already present.
func (s *LockedBucket) upsert(item *Item) error {
	if s == nil {
		return fmt.Errorf("nil storage bucket")
	}

	if item == nil {
		return fmt.Errorf("nil item")
	}

	if item.ID == "" {
		return fmt.Errorf("missing item ID")
	}

	// FIXME: disallow this case to preserve the tree invariant?
	if s.ItemMap == nil {
		s.ItemMap = make(map[string][]byte)
	}

	itemHash := GetItemIDHash(item.ID)

	s.ItemMap[itemHash] = item.Data
	return nil
}

// DeleteItem removes the storage entry which the given key refers to from its
// corresponding bucket.
func (s *StoragePackerV2) DeleteItem(ctx context.Context, itemID string) error {
	if itemID == "" {
		return fmt.Errorf("empty item ID")
	}

	key := GetItemIDHash(itemID)
	bucket, found, err := s.lockBucketForWrite(key)
	if err != nil {
		return err
	}
	if !found {
		bucket, err = s.handleCacheMiss(ctx, key, false, false)
		if err != nil {
			return err
		}
		if bucket == nil {
			return nil
		}
	}
	defer bucket.Unlock()
	if len(bucket.ItemMap) == 0 {
		return nil
	}

	itemHash := GetItemIDHash(itemID)

	_, ok := bucket.ItemMap[itemHash]
	if !ok {
		return nil
	}

	delete(bucket.ItemMap, itemHash)
	return s.storeBucket(ctx, bucket, true)
}

// GetItem fetches the storage entry for a given key from its corresponding
// bucket.
func (s *StoragePackerV2) GetItem(ctx context.Context, itemID string) (*Item, error) {
	if itemID == "" {
		return nil, fmt.Errorf("empty item ID")
	}

	key := GetItemIDHash(itemID)
	bucket, found, err := s.lockBucketForRead(key)
	if err != nil {
		return nil, err
	}
	if found {
		// Have a read-lock in this case
		defer bucket.RUnlock()
	} else {
		// Don't create, don't skip cache
		bucket, err = s.handleCacheMiss(ctx, key, false, false)
		if err != nil {
			return nil, err
		}
		// But a write-lock in this one, FIXME?
		// The write-lock ensures only one read from storage
		defer bucket.Unlock()
	}

	if len(bucket.ItemMap) == 0 {
		return nil, nil
	}

	data, ok := bucket.ItemMap[key]
	if !ok {
		return nil, nil
	}

	return &Item{
		ID:   itemID,
		Data: data,
	}, nil
}

// PutItem stores a storage entry in its corresponding bucket
func (s *StoragePackerV2) PutItem(ctx context.Context, item *Item) error {
	if item == nil {
		return fmt.Errorf("nil item")
	}

	if item.ID == "" {
		return fmt.Errorf("missing ID in item")
	}

	if item.Data == nil {
		return fmt.Errorf("missing data in item")
	}

	if item.Message != nil {
		return fmt.Errorf("'Message' is deprecated; use 'Data' instead")
	}

	key := GetItemIDHash(item.ID)
	bucket, found, err := s.lockBucketForWrite(key)
	if err != nil {
		return err
	}
	if !found {
		// Create, don't skip cache
		bucket, err = s.handleCacheMiss(ctx, key, true, false)
		if err != nil {
			return err
		}
	}
	defer bucket.Unlock()

	if err := bucket.upsert(item); err != nil {
		return errwrap.Wrapf("failed to update entry in packed storage entry: {{err}}", err)
	}

	// Persist the result
	return s.storeBucket(ctx, bucket, true)
}

// NewStoragePackerV2 creates a new storage packer for a given view
func NewStoragePackerV2(ctx context.Context, config *Config) (StoragePacker, error) {
	if config.BucketStorageView == nil {
		return nil, fmt.Errorf("nil buckets view")
	}

	config.BucketStorageView = config.BucketStorageView.SubView("v2/")

	if config.ConfigStorageView == nil {
		return nil, fmt.Errorf("nil config view")
	}

	if config.BaseBucketBits == 0 {
		config.BaseBucketBits = defaultBaseBucketBits
	}

	// At this point, look for an existing saved configuration
	var needPersist bool
	entry, err := config.ConfigStorageView.Get(ctx, "config")
	if err != nil {
		return nil, errwrap.Wrapf("error checking for existing storagepacker config: {{err}}", err)
	}
	if entry != nil {
		needPersist = false
		var exist Config
		if err := entry.DecodeJSON(&exist); err != nil {
			return nil, errwrap.Wrapf("error decoding existing storagepacker config: {{err}}", err)
		}
		// If we have an existing config, we copy the only thing we need
		// constant: the bucket base count, so we know how many to expect at
		// the base level
		//
		// The rest of the values can change
		config.BaseBucketBits = exist.BaseBucketBits
	}

	if config.BucketShardBits == 0 {
		config.BucketShardBits = defaultBucketShardBits
	}

	if config.BaseBucketBits%4 != 0 {
		return nil, fmt.Errorf("bucket base bits of %d is not a multiple of four", config.BaseBucketBits)
	}

	if config.BucketShardBits%4 != 0 {
		return nil, fmt.Errorf("bucket shard count of %d is not a power of two", config.BucketShardBits)
	}

	if config.BaseBucketBits < 4 {
		return nil, errors.New("bucket base bits should be at least 4")
	}
	if config.BucketShardBits < 4 {
		return nil, errors.New("bucket shard count should at least be 4")
	}

	if needPersist {
		entry, err := logical.StorageEntryJSON("config", config)
		if err != nil {
			return nil, errwrap.Wrapf("error encoding storagepacker config: {{err}}", err)
		}
		if err := config.ConfigStorageView.Put(ctx, entry); err != nil {
			return nil, errwrap.Wrapf("error storing storagepacker config: {{err}}", err)
		}
	}

	// Create a new packer object for the given view
	packer := &StoragePackerV2{
		Config:       config,
		bucketsCache: radix.New(),
		storageLocks: locksutil.CreateLocks(),
	}

	// Prewarm the cache
	if _, err := packer.BucketKeys(ctx); err != nil {
		return nil, errwrap.Wrapf("error preloading storagepacker cache: {{err}}", err)
	}

	return packer, nil
}

func (s *StoragePackerV2) SetQueueMode(enabled bool) {
	if enabled {
		atomic.StoreUint32(&s.queueMode, 1)
	} else {
		atomic.StoreUint32(&s.queueMode, 0)
	}
}

func (s *StoragePackerV2) FlushQueue(ctx context.Context) error {
	var err *multierror.Error
	s.queuedBuckets.Range(func(key, value interface{}) bool {
		lErr := s.storeBucket(ctx, value.(*LockedBucket), true)
		if lErr != nil {
			err = multierror.Append(err, lErr)
		}
		s.queuedBuckets.Delete(key)
		return true
	})

	return err.ErrorOrNil()
}
