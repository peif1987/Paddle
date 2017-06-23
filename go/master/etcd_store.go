package master

import (
	"context"
	"sync"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultLockPath is the default etcd master lock path.
	DefaultLockPath = "/master/lock"
	// DefaultStatePath is the default etcd key for master state.
	DefaultStatePath = "/master/state"
)

// Etcd is the etcd abstraction that master uses for fault tolerance
// and service registry.
type Etcd struct {
	lockPath  string
	statePath string
	ttlSec    int
	client    *clientv3.Client

	mu   sync.Mutex
	lock *concurrency.Mutex
}

// NewEtcd creates a new Etcd.
func NewEtcd(endpoints []string, lockPath, statePath string, ttlSec int) (*Etcd, error) {
	// TODO(helin): gracefully shutdown etcd store. Becuase etcd
	// store holds a etcd lock, even though the lock will expire
	// when the lease timeout, we need to implement graceful
	// shutdown to release the lock.
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	})
	if err != nil {
		return nil, err
	}

	sess, err := concurrency.NewSession(cli, concurrency.WithTTL(ttlSec))
	if err != nil {
		return nil, err
	}

	lock := concurrency.NewMutex(sess, lockPath)
	// It's fine for the lock to get stuck, in this case we have
	// multiple master servers running (only configured to have
	// one master running, but split-brain problem may cuase
	// multiple master servers running), and the cluster management
	// software will kill one of them.
	log.Infof("Trying to acquire lock at %s.", lockPath)
	err = lock.Lock(context.TODO())
	if err != nil {
		return nil, err
	}
	log.Infof("Successfully acquired lock at %s.", lockPath)

	e := &Etcd{}
	e.client = cli
	e.lock = lock
	e.lockPath = lockPath
	e.statePath = statePath
	e.ttlSec = ttlSec
	return e, nil
}

// Save saves the state into the etcd.
func (e *Etcd) Save(state []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	ctx := context.TODO()
	put := clientv3.OpPut(e.statePath, string(state))
	resp, err := e.client.Txn(ctx).If(e.lock.IsOwner()).Then(put).Commit()
	if err != nil {
		return err
	}

	if !resp.Succeeded {
		log.Errorln("No longer owns the lock, trying to lock and save again.")
		sess, err := concurrency.NewSession(e.client, concurrency.WithTTL(e.ttlSec))
		if err != nil {
			return err
		}

		e.lock = concurrency.NewMutex(sess, e.lockPath)
		log.Infof("Try to acquire lock at %s.", e.lockPath)
		err = e.lock.Lock(context.TODO())
		if err != nil {
			return err
		}
		log.Infof("Successfully acquired lock at %s.", e.lockPath)
		return e.Save(state)
	}

	return nil
}

// Load loads the state from etcd.
func (e *Etcd) Load() ([]byte, error) {
	e.mu.Lock()
	ctx := context.TODO()
	get := clientv3.OpGet(e.statePath)

	resp, err := e.client.Txn(ctx).If(e.lock.IsOwner()).Then(get).Commit()
	if err != nil {
		return nil, err
	}

	if !resp.Succeeded {
		log.Errorln("No longer owns the lock, trying to lock and load again.")
		sess, err := concurrency.NewSession(e.client)
		if err != nil {
			return nil, err
		}

		e.lock = concurrency.NewMutex(sess, e.lockPath)
		err = e.lock.Lock(context.TODO())
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}

		return e.Load()
	}

	kvs := resp.Responses[0].GetResponseRange().Kvs
	if len(kvs) == 0 {
		// No state exists
		e.mu.Unlock()
		return nil, nil
	}

	state := kvs[0].Value
	e.mu.Unlock()
	return state, nil
}
