package storage

import "errors"

// remoteSynchronized mirrors Magento's RemoteSynchronizedCache (L2 cache):
// every operation is applied to the remote backend first, then to the local one.
// Magento itself leaves local entries alone on a tag clean (they are
// invalidated by the remote "<id>:hash" version key); cleaning them too is
// harmless and frees the space.
type remoteSynchronized struct {
	local, remote Storage
}

func (r *remoteSynchronized) CleanTags(tags []string) error {
	return errors.Join(r.remote.CleanTags(tags), r.local.CleanTags(tags))
}

// remoteHashSuffix is RemoteSynchronizedCache::HASH_SUFFIX: the remote key
// holding the version hash of <id>, removed together with <id>.
const remoteHashSuffix = ":hash"

func (r *remoteSynchronized) CleanIDs(ids []string) error {
	remoteIDs := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		remoteIDs = append(remoteIDs, id, id+remoteHashSuffix)
	}
	return errors.Join(r.remote.CleanIDs(remoteIDs), r.local.CleanIDs(ids))
}

func (r *remoteSynchronized) CleanAll() error {
	return errors.Join(r.remote.CleanAll(), r.local.CleanAll())
}

func (r *remoteSynchronized) Close() error {
	return errors.Join(r.remote.Close(), r.local.Close())
}

func (r *remoteSynchronized) Describe() string {
	return "remote-synchronized (remote: " + r.remote.Describe() + ", local: " + r.local.Describe() + ")"
}
