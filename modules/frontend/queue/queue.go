package queue

import (
	"context"
	"sync"
	"time"

	"github.com/grafana/dskit/services"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"
)

const (
	// How frequently to check for disconnected queriers that should be forgotten.
	forgetCheckPeriod = 5 * time.Second
)

var (
	ErrTooManyRequests = errors.New("too many outstanding requests")
	ErrStopped         = errors.New("queue is stopped")
)

// UserIndex is opaque type that allows to resume iteration over users between successive calls
// of RequestQueue.GetNextRequestForQuerier method.
type UserIndex struct {
	last int
}

// Modify index to start iteration on the same user, for which last queue was returned.
func (ui UserIndex) ReuseLastUser() UserIndex {
	if ui.last >= 0 {
		return UserIndex{last: ui.last - 1}
	}
	return ui
}

// FirstUser returns UserIndex that starts iteration over user queues from the very first user.
func FirstUser() UserIndex {
	return UserIndex{last: -1}
}

// Request stored into the queue.
type Request interface{}

// RequestQueue holds incoming requests in per-user queues. It also assigns each user specified number of queriers,
// and when querier asks for next request to handle (using GetNextRequestForQuerier), it returns requests
// in a fair fashion.
type RequestQueue struct {
	services.Service

	connectedQuerierWorkers *atomic.Int32

	mtx     sync.RWMutex
	cond    contextCond // Notified when request is enqueued or dequeued, or querier is disconnected.
	queues  *queues
	stopped bool

	queueLength       *prometheus.GaugeVec   // Per user and reason.
	discardedRequests *prometheus.CounterVec // Per user.
}

func NewRequestQueue(maxOutstandingPerTenant int, forgetDelay time.Duration, queueLength *prometheus.GaugeVec, discardedRequests *prometheus.CounterVec) *RequestQueue {
	q := &RequestQueue{
		queues:                  newUserQueues(maxOutstandingPerTenant, forgetDelay),
		connectedQuerierWorkers: atomic.NewInt32(0),
		queueLength:             queueLength,
		discardedRequests:       discardedRequests,
	}

	q.cond = contextCond{Cond: sync.NewCond(&q.mtx)}
	q.Service = services.NewTimerService(forgetCheckPeriod, nil, q.forgetDisconnectedQueriers, q.stopping).WithName("request queue")

	return q
}

// EnqueueRequest puts the request into the queue. MaxQueries is user-specific value that specifies how many queriers can
// this user use (zero or negative = all queriers). It is passed to each EnqueueRequest, because it can change
// between calls.
//
// If request is successfully enqueued, successFn is called with the lock held, before any querier can receive the request.
func (q *RequestQueue) EnqueueRequest(userID string, req Request, maxQueriers int) error {
	q.mtx.RLock()
	// don't defer a release. we won't know what we need to release until we call getQueueUnderRlock

	if q.stopped {
		q.mtx.RUnlock()
		return ErrStopped
	}

	// try to grab the user queue under read lock
	queue, cleanup, err := q.getQueueUnderRlock(userID, maxQueriers)
	defer cleanup()
	if err != nil {
		return err
	}

	select {
	case queue <- req:
		q.queueLength.WithLabelValues(userID).Inc()
		q.cond.Broadcast()
		return nil
	default:
		q.discardedRequests.WithLabelValues(userID).Inc()
		return ErrTooManyRequests
	}
}

// getQueueUnderRlock attempts to get the queue for the given user under read lock. if it is not
// possible it upgrades the RLock to a Lock. This method also returns a cleanup function that
// will release whichever lock it had to acquire to get the queue.
func (q *RequestQueue) getQueueUnderRlock(userID string, maxQueriers int) (chan Request, func(), error) {
	cleanup := func() {
		q.mtx.RUnlock()
	}

	uq := q.queues.userQueues[userID]
	if uq != nil {
		return uq.ch, cleanup, nil
	}

	// trade the read lock for a rw lock and then defer the opposite
	// this method should be called under RLock() and return under RLock()
	q.mtx.RUnlock()
	q.mtx.Lock()

	cleanup = func() {
		q.mtx.Unlock()
	}

	queue := q.queues.getOrAddQueue(userID, maxQueriers)
	if queue == nil {
		// This can only happen if userID is "".
		return nil, cleanup, errors.New("no queue found")
	}

	return queue, cleanup, nil
}

// GetNextRequestForQuerier find next user queue and attempts to dequeue N requests as defined by the length of
// batchBuffer. This slice is a reusable buffer to fill up with requests
func (q *RequestQueue) GetNextRequestForQuerier(ctx context.Context, last UserIndex, querierID string, batchBuffer []Request) ([]Request, UserIndex, error) {
	requestedCount := len(batchBuffer)
	if requestedCount == 0 {
		return nil, last, errors.New("batch buffer must have len > 0")
	}

	q.mtx.Lock()
	defer q.mtx.Unlock()

	querierWait := false

FindQueue:
	// We need to wait if there are no users, or no pending requests for given querier.
	for (q.queues.len() == 0 || querierWait) && ctx.Err() == nil && !q.stopped {
		querierWait = false
		q.cond.Wait(ctx)
	}

	if q.stopped {
		return nil, last, ErrStopped
	}

	if err := ctx.Err(); err != nil {
		return nil, last, err
	}

	queue, userID, idx := q.queues.getNextQueueForQuerier(last.last, querierID)
	last.last = idx
	if queue != nil {
		// this is all threadsafe b/c all users queues are blocked by q.mtx
		if len(queue) < requestedCount {
			requestedCount = len(queue)
		}

		// Pick next requests from the queue.
		batchBuffer = batchBuffer[:requestedCount]
		for i := 0; i < requestedCount; i++ {
			batchBuffer[i] = <-queue
		}

		qLen := len(queue)
		if qLen == 0 {
			q.queues.deleteQueue(userID)
		}
		q.queueLength.WithLabelValues(userID).Set(float64(qLen))

		// Tell close() we've processed a request.
		q.cond.Broadcast()

		return batchBuffer, last, nil
	}

	// There are no unexpired requests, so we can get back
	// and wait for more requests.
	querierWait = true
	goto FindQueue
}

func (q *RequestQueue) forgetDisconnectedQueriers(_ context.Context) error {
	q.mtx.Lock()
	defer q.mtx.Unlock()

	if q.queues.forgetDisconnectedQueriers(time.Now()) > 0 {
		// We need to notify goroutines cause having removed some queriers
		// may have caused a resharding.
		q.cond.Broadcast()
	}

	return nil
}

func (q *RequestQueue) stopping(_ error) error {
	q.mtx.Lock()
	defer q.mtx.Unlock()

	for q.queues.len() > 0 && q.connectedQuerierWorkers.Load() > 0 {
		q.cond.Wait(context.Background())
	}

	// Only stop after dispatching enqueued requests.
	q.stopped = true

	// If there are still goroutines in GetNextRequestForQuerier method, they get notified.
	q.cond.Broadcast()

	return nil
}

func (q *RequestQueue) RegisterQuerierConnection(querier string) {
	q.connectedQuerierWorkers.Inc()

	q.mtx.Lock()
	defer q.mtx.Unlock()
	q.queues.addQuerierConnection(querier)
}

func (q *RequestQueue) UnregisterQuerierConnection(querier string) {
	q.connectedQuerierWorkers.Dec()

	q.mtx.Lock()
	defer q.mtx.Unlock()
	q.queues.removeQuerierConnection(querier, time.Now())
}

func (q *RequestQueue) NotifyQuerierShutdown(querierID string) {
	q.mtx.Lock()
	defer q.mtx.Unlock()
	q.queues.notifyQuerierShutdown(querierID)
}

func (q *RequestQueue) GetConnectedQuerierWorkersMetric() float64 {
	return float64(q.connectedQuerierWorkers.Load())
}

// contextCond is a *sync.Cond with Wait() method overridden to support context-based waiting.
type contextCond struct {
	*sync.Cond

	// testHookBeforeWaiting is called before calling Cond.Wait() if it's not nil.
	// Yes, it's ugly, but the http package settled jurisprudence:
	// https://github.com/golang/go/blob/6178d25fc0b28724b1b5aec2b1b74fc06d9294c7/src/net/http/client.go#L596-L601
	testHookBeforeWaiting func()
}

// Wait does c.cond.Wait() but will also return if the context provided is done.
// All the documentation of sync.Cond.Wait() applies, but it's especially important to remember that the mutex of
// the cond should be held while Wait() is called (and mutex will be held once it returns)
func (c contextCond) Wait(ctx context.Context) {
	// "condWait" goroutine does q.cond.Wait() and signals through condWait channel.
	condWait := make(chan struct{})
	go func() {
		if c.testHookBeforeWaiting != nil {
			c.testHookBeforeWaiting()
		}
		c.Cond.Wait()
		close(condWait)
	}()

	// "waiting" goroutine: signals that the condWait goroutine has started waiting.
	// Notice that a closed waiting channel implies that the goroutine above has started waiting
	// (because it has unlocked the mutex), but the other way is not true:
	// - condWait it may have unlocked and is waiting, but someone else locked the mutex faster than us:
	//   in this case that caller will eventually unlock, and we'll be able to enter here.
	// - condWait called Wait(), unlocked, received a broadcast and locked again faster than we were able to lock here:
	//   in this case condWait channel will be closed, and this goroutine will be waiting until we unlock.
	waiting := make(chan struct{})
	go func() {
		c.L.Lock()
		close(waiting)
		c.L.Unlock()
	}()

	select {
	case <-condWait:
		// We don't know whether the waiting goroutine is done or not, but we don't care:
		// it will be done once nobody is fighting for the mutex anymore.
	case <-ctx.Done():
		// In order to avoid leaking the condWait goroutine, we can send a broadcast.
		// Before sending the broadcast we need to make sure that condWait goroutine is already waiting (or has already waited).
		select {
		case <-condWait:
			// No need to broadcast as q.cond.Wait() has returned already.
			return
		case <-waiting:
			// q.cond.Wait() might be still waiting (or maybe not!), so we'll poke it just in case.
			c.Broadcast()
		}

		// Make sure we are not waiting anymore, we need to do that before returning as the caller will need to unlock the mutex.
		<-condWait
	}
}
