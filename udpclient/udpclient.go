package udpclient

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
	"sni/util"
)

type UDPClient struct {
	name string

	conn  *net.UDPConn

	muteLog bool

	isConnected bool
	isClosed    bool

	newRequestsAddedNotified     bool
	newRequestsAddedNotification chan struct{}
	closeWorkerChannel           chan struct{}
	workersDone                  chan struct{}
	incomingMessages             chan []byte

	queuesLock                  sync.Mutex
	inflight                    []*UDPRequestTracker
	queued                      []*UDPRequestTracker
	presumedDroppedResponses    []*UDPPresumedDropped
	udpRetryTimeout             time.Duration
	udpConfidentDroppedTime     time.Duration
	nextTimeout                 time.Time
}

const defaultUDPTries = 3
const defaultUDPRetryTimeout = time.Millisecond*500
const defaultUDPConfidentDroppedTime = time.Second * 5

func NewUDPClient(name string, conn *net.UDPConn) *UDPClient {
	c := &UDPClient{
		name: name,
		conn: conn,
		newRequestsAddedNotification: make(chan struct{}, 1),
		closeWorkerChannel: make(chan struct{}, 5),
		workersDone: make(chan struct{}, 2),
		incomingMessages : make(chan []byte),
		udpRetryTimeout: defaultUDPRetryTimeout,
		udpConfidentDroppedTime: defaultUDPConfidentDroppedTime,
	}
	go c.worker()
	go c.workerHelper()

	c.isConnected = true
	return c
}

func (c *UDPClient) IsClosed() bool { return c.isClosed }

func (c *UDPClient) MuteLog(muted bool) {
	c.muteLog = muted
}

func (c *UDPClient) SetUDPRetryTimeout(duration time.Duration) { c.udpRetryTimeout = duration }
func (c *UDPClient) SetUDPConfidentDroppedTime(duration time.Duration) { c.udpConfidentDroppedTime = duration }

func (c *UDPClient) IsConnected() bool { return c.isConnected }

func (c *UDPClient) log(fmt string, args ...interface{}) {
	if c.muteLog {
		return
	}
	log.Printf(fmt, args...)
}

func (c *UDPClient) LocalAddr() *net.UDPAddr {
	if c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr().(*net.UDPAddr)
}

func (c *UDPClient) RemoteAddr() *net.UDPAddr {
	if c.conn == nil {
		return nil
	}
	return c.conn.RemoteAddr().(*net.UDPAddr)
}

func (c *UDPClient) Close() error {
	if c.isClosed || !c.isConnected {
		return nil
	}

	c.isClosed = true
	c.isConnected = false
	c.closeWorkerChannel <- struct{}{} // tell worker to stop. could also close() on it for a similar effect
	c.conn.Close() // tell workerHelper to stop

	// do cleanup asynchronously
	go func() {
		if c.conn != nil {
			c.conn.Close()
		}
		// wait for both workers to unwind
		<-c.workersDone
		<-c.workersDone
		c.conn = nil

		// complete all outstanding requests
		c.queuesLock.Lock()
		defer c.queuesLock.Unlock()
		for _, rt := range c.inflight {
			rt.complete <- UDPRequestComplete{
				Error: fmt.Errorf("UDPClient has been closed"),
				Answer: nil,
				Request: rt.request,
			}
		}
		for _, rt := range c.queued {
			rt.complete <- UDPRequestComplete{
				Error: fmt.Errorf("UDPClient has been closed"),
				Answer: nil,
				Request: rt.request,
			}
		}
	}()

	return nil
}

type UDPRequestExpectingResponse struct {
	UDPToSend             []byte
	ResponseMustBeginWith []byte
	Retryable             bool
}

type UDPRequestComplete struct {
	Error   error
	Answer  []byte
	Request *UDPRequestExpectingResponse
}

func (c *UDPClient) SendMessageNowExpectingNoResponse(message []byte) error {
	if c.isClosed || !c.isConnected {
		return fmt.Errorf("UDPClient has not connected or is closed")
	} else {
		_, err := c.conn.Write(message)
		return err
	}
}

func (c *UDPClient) StartRequest(ctx context.Context, request *UDPRequestExpectingResponse) chan UDPRequestComplete {
	c.MuteLog(false)
	if c.isClosed || !c.isConnected {
		tempch := make(chan UDPRequestComplete, 1)
		tempch <- UDPRequestComplete{
			Error: fmt.Errorf("UDPClient has not connected or is closed"),
			Answer: nil,
			Request: request,
		}
		return tempch
	}

	rt := &UDPRequestTracker{ctx: ctx,
						request: request,
						complete: make(chan UDPRequestComplete, 1),
						triesFailed: 0}

	conflictDetected := false
	c.queuesLock.Lock()
	{
		for _, presumedDropped := range c.presumedDroppedResponses {
			if c.hasAmbiguity2(rt, presumedDropped) {
				conflictDetected = true
				break
			}
		}
		if !conflictDetected {
			for _, inflightReq := range c.inflight {
				if c.hasAmbiguity(rt, inflightReq) {
					conflictDetected = true
					break
				}
			}
		}
		if !conflictDetected {
			// don't let the new request leapfrog any conflicting requests waiting their turn, as it
			// would delay the waiting request
			for _, queuedReq := range c.queued {
				if c.hasAmbiguity(rt, queuedReq) {
					conflictDetected = true
					break
				}
			}
		}

		if conflictDetected {
			c.queued = append(c.queued, rt)
			// queued can be put inflight when another request completes or times out
		} else {
			c.inflight = append(c.inflight, rt)
			// TODO: should non- Retryable requests get a longer timeout?
			rt.nextTimeout = time.Now().Add(c.udpRetryTimeout)
		}
		if !c.newRequestsAddedNotified {
			c.newRequestsAddedNotification <- struct{}{}
			c.newRequestsAddedNotified = true
		}
	}
	c.queuesLock.Unlock()

	if !conflictDetected {
		// request is ok to send and has been marked 'inflight'. send it now
		c.sendRequest_NoLockHeld(rt)
	}
	return rt.complete
}

// internal:

type UDPRequestTracker struct {
	ctx          context.Context
	request      *UDPRequestExpectingResponse
	complete     chan UDPRequestComplete
	nextTimeout  time.Time
	triesFailed  int
}

type UDPPresumedDropped struct {
	ResponseMustBeginWith  []byte
	forgetAfter            time.Time
	maxCount               int
}

func (c *UDPClient) hasAmbiguity(request1 *UDPRequestTracker, request2 *UDPRequestTracker) bool {
    // if both requests have the same expected response prefix, or one expected response prefix
    // is a shortened version of the other one, they cannot both be inflight at the same time
    if bytes.HasPrefix(request1.request.ResponseMustBeginWith, request2.request.ResponseMustBeginWith) ||
       bytes.HasPrefix(request2.request.ResponseMustBeginWith, request1.request.ResponseMustBeginWith) {

       return true
    }
    return false
}
func (c *UDPClient) hasAmbiguity2(request1 *UDPRequestTracker, request2 *UDPPresumedDropped) bool {
    if bytes.HasPrefix(request1.request.ResponseMustBeginWith,         request2.ResponseMustBeginWith) ||
       bytes.HasPrefix(        request2.ResponseMustBeginWith, request1.request.ResponseMustBeginWith) {

       return true
    }
    return false
}

// caller must ensure request is already stored in the c.inflight object
// on send failure, we don't retry. request is pulled out of c.inflight and completed to sender (thus no direct return value)
func (c *UDPClient) sendRequest_NoLockHeld(rt *UDPRequestTracker) {

	_, err := c.conn.Write([]byte(rt.request.UDPToSend))

	if err != nil {
		c.queuesLock.Lock()
		{
			for i, inflightReq := range c.inflight {
				if inflightReq == rt {
					c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
					break
				}
			}
		}
		c.queuesLock.Unlock()

		rt.complete <- UDPRequestComplete{
			Error: err,
			Answer: nil,
			Request: rt.request,
		}
	}
}

func (c *UDPClient) computeNextTimeout_Nonempty_LockHeld() {
	// find the min time across the time that each thing is expected to happen
	var nextTimeout time.Time
	// pick a starting value to compare everything against
	if len(c.inflight) > 0 {
		nextTimeout = c.inflight[0].nextTimeout
	} else { // we aren't being called unless there's something going on
		nextTimeout = c.presumedDroppedResponses[0].forgetAfter
	}
	for _, presumedDropped := range c.presumedDroppedResponses {
		if presumedDropped.forgetAfter.Before(nextTimeout) {
			nextTimeout = presumedDropped.forgetAfter
		}
	}
	for _, rt := range c.inflight {
		if rt.nextTimeout.Before(nextTimeout) {
			nextTimeout = rt.nextTimeout
		}
		deadline, ok := rt.ctx.Deadline()
		if ok && deadline.Before(nextTimeout) {
			nextTimeout = deadline
		}
	}
	for _, rt := range c.queued {
		deadline, ok := rt.ctx.Deadline()
		if ok && deadline.Before(nextTimeout) {
			nextTimeout = deadline
		}
	}
	c.nextTimeout = nextTimeout
}

func (c *UDPClient) worker() {
	defer util.Recover()
	var rt *UDPRequestTracker

	for {
		var doTimedWait bool
		var inflightRequestsToWrite []*UDPRequestTracker

		c.queuesLock.Lock()
		c.newRequestsAddedNotified = false
		if len(c.presumedDroppedResponses) > 0 || len(c.inflight) > 0 || len(c.queued) > 0 {
			// refresh the data that we manage in case we can get rid of some for being out of date,
			// or need to do a retry, or need to send a new request from the queue
			for i := len(c.presumedDroppedResponses)-1; i >= 0; i-- {
				// the originally-expected packet should have never been this delayed, so start
				// accepting the inflighting of similar requests
				if time.Now().After(c.presumedDroppedResponses[i].forgetAfter) {
					c.presumedDroppedResponses = append(c.presumedDroppedResponses[:i],
					    c.presumedDroppedResponses[i+1:]...)
				}
			}
			for i := len(c.inflight)-1; i >= 0; i-- {
				rt = c.inflight[i]
				if rt.ctx.Err() != nil {
					// sender doesn't want the request anymore
					c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
					c.presumedDroppedResponses = append(c.presumedDroppedResponses, &UDPPresumedDropped{
						ResponseMustBeginWith: rt.request.ResponseMustBeginWith,
						forgetAfter: time.Now().Add(c.udpConfidentDroppedTime),
						maxCount: rt.triesFailed,
					})
					rt.complete <- UDPRequestComplete{
						Error: rt.ctx.Err(),
						Answer: nil,
						Request: rt.request,
					}
				} else if time.Now().After(rt.nextTimeout) {
					rt.triesFailed++
					if rt.triesFailed >= defaultUDPTries || !rt.request.Retryable {
						c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
						c.presumedDroppedResponses = append(c.presumedDroppedResponses, &UDPPresumedDropped{
							ResponseMustBeginWith: rt.request.ResponseMustBeginWith,
							forgetAfter: time.Now().Add(c.udpConfidentDroppedTime),
							maxCount: rt.triesFailed,
						})
						rt.complete <- UDPRequestComplete{
							Error: fmt.Errorf("UDP Request did not get a response within the number of retries"),
							Answer: nil,
							Request: rt.request,
						}
					} else {
						// retry
						rt.nextTimeout = time.Now().Add(c.udpRetryTimeout)
						inflightRequestsToWrite = append(inflightRequestsToWrite, rt)
					}
				}
			}
			for i := len(c.queued)-1; i >= 0; i-- {
				rt = c.queued[i]
				if rt.ctx.Err() != nil {
					// sender doesn't want the request anymore
					c.queued = append(c.queued[:i], c.queued[i+1:]...)
					rt.complete <- UDPRequestComplete{
						Error: rt.ctx.Err(),
						Answer: nil,
						Request: rt.request,
					}
				}
			}
		}
		conflictDetected := false
		for !conflictDetected && len(c.queued) != 0 {
			// prepare to send off the next queued requests if they no longer conflict
			// (happens when the request(s) it was conflicting with have completed or been forgotten).
			// do not send past queued[0] at any time so as to prevent a request from getting
			// permanently stuck by narrower requests leapfrogging it repeatedly
			for _, presumedDropped := range c.presumedDroppedResponses {
				if c.hasAmbiguity2(c.queued[0], presumedDropped) {
					conflictDetected = true
					break
				}
			}
			if !conflictDetected {
				for _, inflightReq := range c.inflight {
					if c.hasAmbiguity(c.queued[0], inflightReq) {
						conflictDetected = true
						break
					}
				}
			}
			if !conflictDetected {
				rt, c.queued = c.queued[0], c.queued[1:]
				c.inflight = append(c.inflight, rt)
				inflightRequestsToWrite = append(inflightRequestsToWrite, rt)
				rt.nextTimeout = time.Now().Add(c.udpRetryTimeout)
			}
		}
		if len(c.inflight) > 0 || len(c.presumedDroppedResponses) > 0 {

			c.computeNextTimeout_Nonempty_LockHeld()
			doTimedWait = true

		} else {
			doTimedWait = false
		}

		c.queuesLock.Unlock()

		for _, rt = range inflightRequestsToWrite {
			c.sendRequest_NoLockHeld(rt)
		}

		// sleep until: class closed, new incoming messages, new outgoing messages sent, or (if we
		// know of a timeout) the next timeout
		if doTimedWait {
			nowToCompare := time.Now()
			if time.Now().Before(c.nextTimeout) {
				select {
				case <-c.closeWorkerChannel:
					c.workersDone <- struct{}{}
					return
				case message := <-c.incomingMessages:
					c.handleIncomingMessage(message)
				case <-c.newRequestsAddedNotification:
					// go to start of loop and reprocess timeouts
				case <-time.After(c.nextTimeout.Sub(nowToCompare)):
					// go to start of loop and reprocess timeouts
				}
			}
		} else {
			select {
			case <-c.closeWorkerChannel:
				c.workersDone <- struct{}{}
				return
			case message := <-c.incomingMessages:
				c.handleIncomingMessage(message)
			case <-c.newRequestsAddedNotification:
				// go to start of loop and reprocess timeouts
			}
		}
	}
}

func (c *UDPClient) handleIncomingMessage(message []byte) {
	c.queuesLock.Lock()
	defer c.queuesLock.Unlock()
	for i, presumedDropped := range c.presumedDroppedResponses {
		if bytes.HasPrefix(message, presumedDropped.ResponseMustBeginWith) {
			presumedDropped.maxCount--
			if presumedDropped.maxCount == 0 {
				c.presumedDroppedResponses = append(c.presumedDroppedResponses[:i],
					c.presumedDroppedResponses[i+1:]...)
			}
			return // message can't match both a dropped packet expectation and an inflight request expectation
		}
	}
	for i, rt := range c.inflight {
		if bytes.HasPrefix(message, rt.request.ResponseMustBeginWith) {
			c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
			if rt.triesFailed != 0 {
				// we sent this request more than once, so more responses might come in
				c.presumedDroppedResponses = append(c.presumedDroppedResponses, &UDPPresumedDropped{
					ResponseMustBeginWith: rt.request.ResponseMustBeginWith,
					forgetAfter: time.Now().Add(c.udpConfidentDroppedTime),
					maxCount: rt.triesFailed,
				})
			}
			// complete the request successfully!
			rt.complete <- UDPRequestComplete{
				Error: nil,
				Answer: message,
				Request: rt.request,
			}
			return
		}
	}
	c.log("%s: Dropping unexpected incoming UDP packet. its length was %d\n", c.name, len(message))
}

func (c *UDPClient) workerHelper() {
	c.conn.SetReadDeadline(time.Time{}) // disable any timeout
	for {
		message := make([]byte, 65536)
		n, err := c.conn.Read(message) // sleep here until data comes in or the class gets closed
		if err != nil {
			if c.isClosed {
				break // move on to cleanup
			} else {
				log.Printf("%s: closing due to unexpected UDP Read error %v", c.name, err)
				c.Close()
				break
			}
		}
		message = message[:n]
		c.incomingMessages <- message // sleep here until worker is ready for data
	}

	c.workersDone <- struct{}{}
}

// TODO remember to strings.TrimSpace()
